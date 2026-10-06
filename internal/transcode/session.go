package transcode

// session.go: ffmpeg HLS sessions (TASKS P2.5, DESIGN §8.3), adapted from
// jollyrogarr's session manager. A session is one ffmpeg process writing
// numbered segments ({n}.ts) of one play session into its own directory.
//
// Changes from jollyrogarr:
//   - Input is a local path or a remote URL (ffmpeg reconnects on drops).
//   - Streams are mapped explicitly (video + the chosen audio track; HLS TS
//     can't carry the source's subtitle streams), and video and audio are
//     copied or encoded independently, as media.Decide chose.
//   - Encodes target the client's bitrate (capped VBR), not a fixed
//     quality, and force a keyframe every segment so segment n always
//     starts at n×SegmentSeconds: the HLS playlist is written up front from
//     the duration (P2.6) and a seek restarts ffmpeg at a segment boundary.
//   - Segments appear only when complete (hls_flags temp_file), and a
//     restart keeps the segments already written (same timeline).
//   - The manager enforces transcode.max_sessions and reaps sessions idle
//     for IdleTimeout (DESIGN: 60s) itself.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// DefaultSegmentSeconds is the HLS segment length (transcode.segment_seconds).
const DefaultSegmentSeconds = 3 // as Jellyfin 12.1.0 (actualSegmentLengthTicks=30000000)

// PlaylistName is ffmpeg's own playlist in a session directory; clients get
// the one P2.6 generates up front, this one only records progress.
const PlaylistName = "index.m3u8"

// SessionState is a transcode session's lifecycle state.
type SessionState string

const (
	StateStarting SessionState = "starting" // ffmpeg launched, first segment not ready yet
	StateRunning  SessionState = "running"  // segments are being written
	StateDone     SessionState = "done"     // ffmpeg reached the end of the input
	StateFailed   SessionState = "failed"   // ffmpeg exited with an error
)

var (
	// ErrSegmentTimeout means ffmpeg didn't produce the segment in time.
	ErrSegmentTimeout = errors.New("transcode: ffmpeg did not produce the segment in time")
	// ErrTooManySessions means transcode.max_sessions are already running.
	ErrTooManySessions = errors.New("transcode: too many transcode sessions")
	// ErrBadSessionID means a session id isn't safe as a directory name.
	ErrBadSessionID = errors.New("transcode: invalid session id")
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// StartOptions configures one session.
type StartOptions struct {
	Input  string // local file path or http(s) URL
	Remote bool   // Input is a URL: reconnect on network errors

	// Video is copied (remux) or encoded with Encoder.
	CopyVideo    bool
	Encoder      Capability // from Choose
	Device       string     // DRI render node for qsv/vaapi
	VideoCodec   string     // "h264" (default) or "hevc"
	VideoBitrate int64      // bits/s target; 0 = the encoder's quality default
	Tonemap      bool       // HDR → SDR before encoding (CPU filter, hardware encode)

	// AudioStream is the source stream index to play; -1 = the first audio.
	AudioStream   int
	CopyAudio     bool
	AudioCodec    string // "aac" (default), "mp3", "ac3", …
	AudioBitrate  int64  // bits/s; 0 = the encoder's default
	AudioChannels int    // downmix to this many; 0 = keep

	SegmentSeconds int // <= 0: DefaultSegmentSeconds
	StartSegment   int // first segment number; ffmpeg starts reading at StartSegment×SegmentSeconds

	Owner string // who the session is for (a device id); not passed to ffmpeg
}

func (o StartOptions) segmentSeconds() int {
	if o.SegmentSeconds <= 0 {
		return DefaultSegmentSeconds
	}
	return o.SegmentSeconds
}

// tonemapChain converts HDR (PQ/HLG, BT.2020) to SDR BT.709 on the CPU
// before a hardware encode, like Jellyfin/Plex's hybrid (from jollyrogarr:
// encoding HDR values as SDR H.264 gives the washed-out look).
const tonemapChain = "zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

func encoderName(c Capability, codec string) string {
	if codec != "hevc" {
		codec = "h264"
	}
	switch c {
	case CapQSV:
		return codec + "_qsv"
	case CapVAAPI:
		return codec + "_vaapi"
	case CapNVENC:
		return codec + "_nvenc"
	}
	if codec == "hevc" {
		return "libx265"
	}
	return "libx264"
}

func buildArgs(o StartOptions, dir string) []string {
	seg := o.segmentSeconds()
	start := float64(o.StartSegment * seg)
	args := []string{"-y", "-v", "error", "-nostdin"}
	if o.Remote {
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-reconnect_on_network_error", "1", "-reconnect_delay_max", "5")
	}
	if start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	if !o.CopyVideo {
		switch o.Encoder {
		case CapVAAPI:
			args = append(args, "-init_hw_device", "vaapi=va:"+o.Device, "-filter_hw_device", "va")
		case CapQSV:
			args = append(args, "-init_hw_device", "qsv=qs:"+o.Device, "-filter_hw_device", "qs")
		}
	}
	args = append(args, "-i", o.Input, "-map", "0:v:0")
	if o.AudioStream >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(o.AudioStream))
	} else {
		args = append(args, "-map", "0:a:0?")
	}

	if o.CopyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		// vf joins the optional tonemap with the encoder's upload filter.
		vf := func(suffix string) string {
			switch {
			case !o.Tonemap:
				return suffix
			case suffix == "":
				return tonemapChain
			}
			return tonemapChain + "," + suffix
		}
		filter := ""
		switch o.Encoder {
		case CapVAAPI:
			filter = vf("format=nv12,hwupload")
		case CapQSV:
			filter = vf("format=nv12,hwupload=extra_hw_frames=64")
		case CapNVENC:
			filter = vf("")
		default:
			filter = vf("format=yuv420p") // 8-bit output from 10-bit sources
		}
		if filter != "" {
			args = append(args, "-vf", filter)
		}
		args = append(args, "-c:v", encoderName(o.Encoder, o.VideoCodec))
		if o.VideoBitrate > 0 {
			b := strconv.FormatInt(o.VideoBitrate, 10)
			args = append(args, "-b:v", b, "-maxrate", b, "-bufsize", strconv.FormatInt(2*o.VideoBitrate, 10))
		} else {
			switch o.Encoder {
			case CapVAAPI:
				args = append(args, "-qp", "23")
			case CapQSV:
				args = append(args, "-global_quality", "23")
			case CapNVENC:
				args = append(args, "-cq", "23")
			default:
				args = append(args, "-crf", "21")
			}
		}
		if o.Encoder == CapSoftware || o.Encoder == "" {
			args = append(args, "-preset", "veryfast")
		}
		// A keyframe at every segment boundary: segment n starts at n×seg.
		// QSV and NVENC make forced keyframes plain I-frames unless told to
		// use IDR, and the HLS muxer only cuts on IDR (seen live: 10.7s
		// segments from h264_qsv without it).
		args = append(args, "-force_key_frames", "expr:gte(t,n_forced*"+strconv.Itoa(seg)+")")
		switch o.Encoder {
		case CapQSV:
			args = append(args, "-forced_idr", "1")
		case CapNVENC:
			args = append(args, "-forced-idr", "1")
		}
	}

	if o.CopyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		codec := o.AudioCodec
		if codec == "" {
			codec = "aac"
		}
		args = append(args, "-c:a", codec)
		if o.AudioBitrate > 0 {
			args = append(args, "-b:a", strconv.FormatInt(o.AudioBitrate, 10))
		}
		if o.AudioChannels > 0 {
			args = append(args, "-ac", strconv.Itoa(o.AudioChannels))
		}
	}

	if start > 0 {
		// Continue the original timeline, so a restarted session reads to
		// the player as the same stream (from jollyrogarr).
		args = append(args, "-output_ts_offset", strconv.FormatFloat(start, 'f', 3, 64))
	}
	return append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(seg),
		"-hls_list_size", "0",
		"-hls_segment_type", "mpegts",
		"-hls_flags", "temp_file", // a segment file appears only once complete
		"-start_number", strconv.Itoa(o.StartSegment),
		"-hls_segment_filename", filepath.Join(dir, "%d.ts"),
		filepath.Join(dir, PlaylistName),
	)
}

// runFFmpegSession is a var so tests can stub out the real ffmpeg process.
var runFFmpegSession = func(ctx context.Context, args []string, stderr *bytes.Buffer) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// pollInterval is a var so tests can shrink it.
var pollInterval = 100 * time.Millisecond

// Session is one ffmpeg run for a play session.
type Session struct {
	ID   string
	Dir  string
	Opts StartOptions

	cancel context.CancelFunc
	done   chan struct{} // closed once ffmpeg has exited

	mu       sync.Mutex
	state    SessionState
	err      error
	lastUsed time.Time
}

// Touch marks the session as in use (resets its idle timer).
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *Session) idleSince() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

// State returns the lifecycle state and, when failed, why.
func (s *Session) State() (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.err
}

func (s *Session) setState(st SessionState, err error) {
	s.mu.Lock()
	s.state, s.err = st, err
	s.mu.Unlock()
}

// SegmentPath is where segment n is (or will be) written.
func (s *Session) SegmentPath(n int) string { return filepath.Join(s.Dir, strconv.Itoa(n)+".ts") }

// HasSegment reports whether segment n is complete on disk.
func (s *Session) HasSegment(n int) bool {
	fi, err := os.Stat(s.SegmentPath(n))
	return err == nil && fi.Size() > 0
}

// Next is the first segment from StartSegment on that isn't written yet:
// where ffmpeg currently is.
func (s *Session) Next() int {
	n := s.Opts.StartSegment
	for s.HasSegment(n) {
		n++
	}
	return n
}

// WaitSegment waits until segment n is complete, ffmpeg fails or finishes
// without it, or timeout passes.
func (s *Session) WaitSegment(ctx context.Context, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if s.HasSegment(n) {
			return nil
		}
		select {
		case <-s.done:
			if s.HasSegment(n) {
				return nil
			}
			if st, err := s.State(); st == StateFailed {
				return err
			}
			return fmt.Errorf("transcode: segment %d not produced (input ended)", n)
		default:
		}
		if time.Now().After(deadline) {
			return ErrSegmentTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// stop kills ffmpeg and waits for it to exit; the directory stays.
func (s *Session) stop() {
	s.cancel()
	<-s.done
}

// Manager starts, tracks and stops sessions.
type Manager struct {
	baseDir     string
	maxSessions int // 0 = unlimited
	// IdleTimeout closes sessions nobody fetched a segment from for this
	// long (Run). Set before Run.
	IdleTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager writes session directories under baseDir and allows at most
// maxSessions at a time (0 = unlimited).
func NewManager(baseDir string, maxSessions int) *Manager {
	return &Manager{baseDir: baseDir, maxSessions: maxSessions, IdleTimeout: time.Minute, sessions: map[string]*Session{}}
}

// RemoveStale deletes session directories left by a previous run.
func (m *Manager) RemoveStale() error {
	entries, err := os.ReadDir(m.baseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(m.baseDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Start launches ffmpeg for session id (replacing a running one with that
// id) and waits up to startupTimeout for its first segment. Segments a
// previous run of the same id already wrote are kept.
func (m *Manager) Start(ctx context.Context, id string, opts StartOptions, startupTimeout time.Duration) (*Session, error) {
	if !validID.MatchString(id) {
		return nil, ErrBadSessionID
	}
	m.mu.Lock()
	old := m.sessions[id]
	if old == nil && m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		m.mu.Unlock()
		return nil, ErrTooManySessions
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	if old != nil {
		old.stop()
	}

	dir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: create session dir: %w", err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	s := &Session{ID: id, Dir: dir, Opts: opts, cancel: cancel, done: make(chan struct{}), state: StateStarting}
	s.Touch()
	var stderr bytes.Buffer
	cmd, err := runFFmpegSession(sctx, buildArgs(opts, dir), &stderr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("transcode: start ffmpeg: %w", err)
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	go func() {
		defer close(s.done)
		err := cmd.Wait()
		switch {
		case sctx.Err() != nil: // stopped by us
		case err != nil:
			s.setState(StateFailed, fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(stderr.Bytes())))
		default:
			s.setState(StateDone, nil)
		}
	}()

	if err := s.WaitSegment(ctx, opts.StartSegment, startupTimeout); err != nil {
		m.Close(id)
		return nil, err
	}
	if st, _ := s.State(); st == StateStarting {
		s.setState(StateRunning, nil)
	}
	return s, nil
}

// Restart relaunches session id from segment n with the options it was
// started with (a seek ahead of or behind what ffmpeg has written).
func (m *Manager) Restart(ctx context.Context, id string, n int, startupTimeout time.Duration) (*Session, bool, error) {
	s, ok := m.Get(id)
	if !ok {
		return nil, false, nil
	}
	opts := s.Opts
	opts.StartSegment = n
	s, err := m.Start(ctx, id, opts, startupTimeout)
	return s, true, err
}

// Get returns a session and marks it used.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if ok {
		s.Touch()
	}
	return s, ok
}

// Count is the number of sessions.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Close stops session id and deletes its directory.
func (m *Manager) Close(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if s != nil {
		s.stop()
		_ = os.RemoveAll(s.Dir)
	}
}

// CloseOwner stops every session of owner.
func (m *Manager) CloseOwner(owner string) {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		if s.Opts.Owner == owner {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
}

// CloseAll stops every session.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Close(id)
	}
}

// Run closes idle sessions until ctx ends, then closes them all.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(max(m.IdleTimeout/4, 10*time.Millisecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.CloseAll()
			return
		case now := <-t.C:
			m.closeIdle(now)
		}
	}
}

func (m *Manager) closeIdle(now time.Time) {
	m.mu.Lock()
	var idle []string
	for id, s := range m.sessions {
		if now.Sub(s.idleSince()) >= m.IdleTimeout {
			idle = append(idle, id)
		}
	}
	m.mu.Unlock()
	for _, id := range idle {
		m.Close(id)
	}
}
