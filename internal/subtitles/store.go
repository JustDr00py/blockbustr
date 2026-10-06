// Package subtitles serves text subtitles as files (TASKS P2.8, DESIGN
// §8.3): embedded tracks are extracted once into a disk cache (all text
// tracks of a source in one ffmpeg pass, since every extraction reads the
// whole file, possibly over the network) and converted on request to SRT,
// WebVTT or ASS. Sidecar files are used as they are.
package subtitles

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

// ExtractTimeout bounds one extraction pass (it reads the whole file).
const ExtractTimeout = 10 * time.Minute

// ErrNotText means the track isn't a text subtitle (PGS, VobSub…): those
// can only be burned in.
var ErrNotText = errors.New("subtitles: not a text subtitle")

// textCodecs are the text subtitle codecs, with the file format each is
// cached in.
var textCodecs = map[string]string{
	"subrip": "srt", "srt": "srt", "ass": "ass", "ssa": "ssa", "webvtt": "vtt", "mov_text": "srt", "text": "srt",
}

// IsText reports whether a subtitle codec can be served as a file.
func IsText(codec string) bool { return textCodecs[strings.ToLower(codec)] != "" }

// Track is one subtitle stream of a source.
type Track struct {
	Index int
	Codec string
	Path  string // sidecar file; "" for a track inside the media file
}

// Source is where a source's embedded tracks are read from.
type Source struct {
	Key    string // changes when the media changes (e.g. source id + etag)
	Input  string // local path or URL
	Remote bool
	Tracks []Track // every subtitle track of the source
}

// Store caches extracted tracks under Dir.
type Store struct {
	Dir string
	sf  singleflight.Group
}

// runFFmpeg is a var so tests can count ffmpeg runs.
var runFFmpeg = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func (s *Store) cached(src Source, t Track) string {
	return filepath.Join(s.Dir, src.Key, strconv.Itoa(t.Index)+"."+textCodecs[strings.ToLower(t.Codec)])
}

// File returns a text file holding track index: the sidecar itself, or the
// cached extract (extracting every embedded text track of src first when
// needed; concurrent callers share one pass).
func (s *Store) File(ctx context.Context, src Source, index int) (string, error) {
	var track *Track
	for i := range src.Tracks {
		if src.Tracks[i].Index == index {
			track = &src.Tracks[i]
		}
	}
	switch {
	case track == nil:
		return "", fmt.Errorf("subtitles: no track %d", index)
	case !IsText(track.Codec):
		return "", ErrNotText
	case track.Path != "":
		return track.Path, nil
	}
	p := s.cached(src, *track)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	_, err, _ := s.sf.Do(src.Key, func() (any, error) {
		return nil, s.extract(context.WithoutCancel(ctx), src)
	})
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("subtitles: track %d not extracted", index)
	}
	return p, nil
}

// extract writes every embedded text track of src in one ffmpeg pass.
func (s *Store) extract(ctx context.Context, src Source) error {
	ctx, cancel := context.WithTimeout(ctx, ExtractTimeout)
	defer cancel()
	dir := filepath.Join(s.Dir, src.Key)
	tmp, err := os.MkdirTemp(s.Dir, ".extract-")
	if err != nil {
		if err := os.MkdirAll(s.Dir, 0o755); err != nil {
			return err
		}
		if tmp, err = os.MkdirTemp(s.Dir, ".extract-"); err != nil {
			return err
		}
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	args := []string{"-y", "-v", "error", "-nostdin"}
	if src.Remote {
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "5")
	}
	args = append(args, "-i", src.Input)
	var outs []Track
	for _, t := range src.Tracks {
		if t.Path != "" || !IsText(t.Codec) {
			continue
		}
		codec := "copy"
		if c := strings.ToLower(t.Codec); c == "mov_text" || c == "text" {
			codec = "srt" // re-encoded to SubRip; the rest is copied as is
		}
		args = append(args, "-map", "0:"+strconv.Itoa(t.Index), "-c:s", codec, filepath.Join(tmp, filepath.Base(s.cached(src, t))))
		outs = append(outs, t)
	}
	if len(outs) == 0 {
		return nil
	}
	if _, err := runFFmpeg(ctx, args...); err != nil {
		return fmt.Errorf("subtitles: extract: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, t := range outs {
		name := filepath.Base(s.cached(src, t))
		if err := os.Rename(filepath.Join(tmp, name), filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// Copy copies file into the store under a name derived from its path and
// contents (for libass, whose filter options can't take every file name).
func (s *Store) Copy(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(file+"\x00"), data...))
	dst := filepath.Join(s.Dir, "burn", hex.EncodeToString(sum[:12])+strings.ToLower(filepath.Ext(file)))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	return dst, os.Rename(tmp, dst)
}

// Formats are the output formats Convert takes, by request extension.
var Formats = map[string]string{"srt": "srt", "subrip": "srt", "vtt": "webvtt", "webvtt": "webvtt", "ass": "ass", "ssa": "ass"}

// Convert returns file in format (a Formats value). With start > 0, cues
// ending before it are dropped and the rest shifted to start from it
// (ffmpeg's -ss snaps to an earlier cue on subtitle inputs, so the shift is
// done here).
func Convert(ctx context.Context, file, format string, start time.Duration) ([]byte, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(file)), ".")
	var data []byte
	var err error
	if (ext == "srt" && format == "srt") || (ext == "vtt" && format == "webvtt") || ((ext == "ass" || ext == "ssa") && format == "ass") {
		data, err = os.ReadFile(file)
	} else {
		data, err = runFFmpeg(ctx, "-v", "error", "-nostdin", "-i", file, "-f", format, "-")
	}
	if err != nil || start <= 0 {
		return data, err
	}
	return shiftCues(data, format, start), nil
}

var (
	srtTime  = regexp.MustCompile(`^(\d+):(\d{2}):(\d{2})[,.](\d{3}) --> (\d+):(\d{2}):(\d{2})[,.](\d{3})(.*)$`)
	vttTime  = regexp.MustCompile(`^(?:(\d+):)?(\d{2}):(\d{2})\.(\d{3}) --> (?:(\d+):)?(\d{2}):(\d{2})\.(\d{3})(.*)$`)
	assEvent = regexp.MustCompile(`^(Dialogue:\s*[^,]*,)(\d+):(\d{2}):(\d{2})\.(\d{2}),(\d+):(\d{2}):(\d{2})\.(\d{2}),(.*)$`)
)

func hmsDuration(h, m, s, frac string, fracUnit time.Duration) time.Duration {
	n := func(v string) time.Duration { x, _ := strconv.Atoi(v); return time.Duration(x) }
	return n(h)*time.Hour + n(m)*time.Minute + n(s)*time.Second + n(frac)*fracUnit
}

// shiftCues drops cues ending at or before start and moves the rest back by
// start (a cue running across it begins at 0).
func shiftCues(data []byte, format string, start time.Duration) []byte {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	shift := func(a, b time.Duration) (time.Duration, time.Duration, bool) {
		if b <= start {
			return 0, 0, false
		}
		return max(a-start, 0), b - start, true
	}
	var out []string
	switch format {
	case "ass":
		for _, l := range lines {
			m := assEvent.FindStringSubmatch(l)
			if m == nil {
				out = append(out, l)
				continue
			}
			a, b, ok := shift(hmsDuration(m[2], m[3], m[4], m[5], 10*time.Millisecond), hmsDuration(m[6], m[7], m[8], m[9], 10*time.Millisecond))
			if ok {
				out = append(out, m[1]+assTime(a)+","+assTime(b)+","+m[10])
			}
		}
	default: // srt, webvtt: blocks separated by blank lines
		re, sep := srtTime, ","
		if format == "webvtt" {
			re, sep = vttTime, "."
		}
		n := 0
		for i := 0; i < len(lines); {
			j := i
			for j < len(lines) && lines[j] != "" {
				j++
			}
			block := lines[i:j]
			keep, timed := true, false
			for k, l := range block {
				m := re.FindStringSubmatch(l)
				if m == nil {
					continue
				}
				timed = true
				a, b, ok := shift(hmsDuration(m[1], m[2], m[3], m[4], time.Millisecond), hmsDuration(m[5], m[6], m[7], m[8], time.Millisecond))
				if !ok {
					keep = false
					break
				}
				block[k] = cueTime(a, sep) + " --> " + cueTime(b, sep) + m[9]
				if format == "srt" && k > 0 { // renumber
					n++
					block[k-1] = strconv.Itoa(n)
				}
			}
			if keep || !timed {
				out = append(out, block...)
				if j < len(lines) {
					out = append(out, "")
				}
			}
			i = j + 1
		}
	}
	return []byte(strings.Join(out, "\n"))
}

func cueTime(d time.Duration, sep string) string {
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, sep, d.Milliseconds()%1000)
}

func assTime(d time.Duration) string {
	return fmt.Sprintf("%d:%02d:%02d.%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, (d.Milliseconds()%1000)/10)
}

// ContentType is the MIME type of a Formats value.
func ContentType(format string) string {
	switch format {
	case "webvtt":
		return "text/vtt; charset=utf-8"
	case "ass":
		return "text/x-ssa; charset=utf-8"
	}
	return "application/x-subrip; charset=utf-8"
}
