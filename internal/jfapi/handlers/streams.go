package handlers

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/language"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// Catalog titles have no stored media sources: their streams come from the
// addons when played (TASKS P3.7, DESIGN §7.3). PlaybackInfo collects and
// ranks them and offers the best few as MediaSources; the choices are kept
// in Redis (streamset:{item}) so the stream, HLS and subtitle endpoints find
// the one a client picked, and item details can list them as versions.
// Subtitle addons' subtitles (P3.9) ride along as external tracks of every
// choice.

// StreamCollector gathers the streams addons offer for a title
// (stremio.Collector).
type StreamCollector interface {
	Collect(ctx context.Context, typ, id string) ([]stremio.Offer, error)
	Subtitles(ctx context.Context, typ, id string) ([]stremio.SubtitleOffer, error)
}

// StreamChoice is one addon stream offered as a media source.
type StreamChoice struct {
	ID        uuid.UUID // the media source id; stable for the same stream
	Name      string    // the label clients show ("1080p HEVC • 2.4 GB • Torrentio")
	Target    string    // for resolve.FromURL: the stream URL, or resolve.Magnet
	Container string    // from the file name; "" when unknown
	Size      int64
}

// SubtitleChoice is one addon subtitle offered as an external track.
type SubtitleChoice struct {
	Language string // ISO 639-2, as ffprobe reports embedded tracks
	URL      string
	Addon    string
}

// streamSet is what streamset:{item} holds: the latest offer first, then
// earlier offers a client may still be playing, and the title's subtitles.
type streamSet struct {
	Choices   []StreamChoice
	Subtitles []SubtitleChoice
}

// firstAddonSubtitle is the index of a choice's first addon subtitle,
// leaving the low indexes to its own tracks once it's probed.
const firstAddonSubtitle = 100

// maxStreamSet bounds how many choices are remembered per item.
const maxStreamSet = 12

// stremioRef is the addon type and id a catalog item's streams are asked
// for: its path is stremio:{type}:{id} ("stremio:series:tt0944947:1:2").
func stremioRef(it db.Item) (typ, id string, ok bool) {
	if it.SourceKind != "stremio" || it.Path == nil {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(*it.Path, "stremio:")
	if !ok {
		return "", "", false
	}
	typ, id, ok = strings.Cut(rest, ":")
	return typ, id, ok && id != ""
}

// choiceSources are the sources a set's choices stand for, in its order,
// and their streams: the set's subtitles, as external text tracks.
func choiceSources(it db.Item, choices []StreamChoice, subs []SubtitleChoice) ([]db.MediaSource, map[uuid.UUID][]db.MediaStream) {
	out := make([]db.MediaSource, 0, len(choices))
	streams := map[uuid.UUID][]db.MediaStream{}
	for _, c := range choices {
		for i, sub := range subs {
			streams[c.ID] = append(streams[c.ID], db.MediaStream{
				MediaSourceID: c.ID, Idx: int32(firstAddonSubtitle + i), Type: "Subtitle", Codec: ptr("subrip"),
				Language: ptr(sub.Language), Title: ptr(sub.Addon), IsExternal: true, ExternalPath: ptr(sub.URL),
			})
		}
		src := db.MediaSource{
			ID: c.ID, ItemID: it.ID, Protocol: "Http", IsRemote: true, PathOrUrl: c.Target,
			Name: c.Name, RuntimeTicks: it.RuntimeTicks, Etag: it.Etag,
		}
		if c.Container != "" {
			src.Container = ptr(c.Container)
		}
		if c.Size > 0 {
			src.Size = ptr(c.Size)
			if it.RuntimeTicks != nil && *it.RuntimeTicks > 0 {
				// An estimate from the size; probing at first play replaces it.
				src.Bitrate = ptr(int32(min(c.Size*8*10_000_000 / *it.RuntimeTicks, 1<<31-1)))
			}
		}
		out = append(out, src)
	}
	return out, streams
}

func (a *api) loadStreamSet(ctx context.Context, item uuid.UUID) (streamSet, bool) {
	var set streamSet
	if a.Cache == nil {
		return set, false
	}
	ok, err := a.Cache.GetJSON(ctx, cache.StreamSetKey(item.String()), &set)
	return set, err == nil && ok && len(set.Choices) > 0
}

// addStreamChoices gives a catalog item without stored sources its stream
// choices. With collect, a title with none remembered (never played, or
// expired) is collected first, for prefs.
func (a *api) addStreamChoices(ctx context.Context, b *itemBatch, it db.Item, collect bool, prefs stremio.Prefs) error {
	if _, _, ok := stremioRef(it); !ok || len(b.sources[it.ID]) > 0 {
		return nil
	}
	set, ok := a.loadStreamSet(ctx, it.ID)
	if !ok && collect {
		var err error
		if set, err = a.pickStreams(ctx, it, prefs); err != nil {
			return err
		}
	}
	if len(set.Choices) > 0 {
		var streams map[uuid.UUID][]db.MediaStream
		b.sources[it.ID], streams = choiceSources(it, set.Choices, set.Subtitles)
		for id, st := range streams {
			b.streams[id] = st
		}
	}
	return nil
}

// loadPlaySources loads the sources a stream request picks from: the
// stored ones, or a catalog item's stream choices. Choices that expired
// (the client resumed long after PlaybackInfo) are collected again, for a
// client that can play anything.
func (a *api) loadPlaySources(ctx context.Context, b *itemBatch, it db.Item) error {
	if err := a.loadSources(ctx, b, []uuid.UUID{it.ID}); err != nil {
		return err
	}
	return a.addStreamChoices(ctx, b, it, true, streamPrefs(media.DeviceProfile{}, 0))
}

// pickStreams collects and ranks a catalog item's streams for prefs, and
// its subtitles, in parallel; remembers the best few streams
// (stremio.streams.top) ahead of earlier choices; and returns this pick:
// the streams best first, and the subtitles. No streams is not an error:
// the item keeps its placeholder source.
func (a *api) pickStreams(ctx context.Context, it db.Item, prefs stremio.Prefs) (streamSet, error) {
	typ, id, ok := stremioRef(it)
	if !ok || a.Streams == nil {
		return streamSet{}, nil
	}
	var subs []stremio.SubtitleOffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		if subs, err = a.Streams.Subtitles(ctx, typ, id); err != nil {
			a.Log.WarnContext(ctx, "subtitle collection failed", "item", it.ID, "err", err)
		}
	}()
	offers, err := a.Streams.Collect(ctx, typ, id)
	<-done
	if err != nil {
		return streamSet{}, err
	}
	cfg := a.Config.Stremio.Streams
	prefs.Languages, prefs.Allow, prefs.Deny = cfg.Languages, cfg.AllowGroups, cfg.DenyGroups
	if prefs.Runtime == 0 && it.RuntimeTicks != nil {
		prefs.Runtime = time.Duration(*it.RuntimeTicks) * 100 // ticks are 100 ns
	}
	ranked := stremio.Rank(offers, prefs)
	top := max(cfg.Top, 1)
	choices := make([]StreamChoice, 0, top)
	bad := a.badChoices(ctx, it.ID)
	for _, r := range ranked {
		if len(choices) == top {
			break
		}
		if c := streamChoice(it.ID, r); !bad[c.ID] {
			choices = append(choices, c)
		}
	}
	pick := streamSet{Choices: choices, Subtitles: pickSubtitles(subs, a.Config.Stremio.Subtitles.Languages, a.Config.Stremio.Subtitles.PerLanguage)}
	if len(choices) == 0 || a.Cache == nil {
		return pick, nil
	}
	set, _ := a.loadStreamSet(ctx, it.ID)
	merged := append([]StreamChoice{}, choices...)
	for _, c := range set.Choices {
		if len(merged) >= maxStreamSet {
			break
		}
		if !hasChoice(merged, c.ID) {
			merged = append(merged, c)
		}
	}
	if err := a.Cache.SetJSON(ctx, cache.StreamSetKey(it.ID.String()), streamSet{Choices: merged, Subtitles: pick.Subtitles}, cache.StreamSetTTL); err != nil {
		return streamSet{}, err
	}
	return pick, nil
}

// maxAddonSubtitles bounds the subtitles offered per title.
const maxAddonSubtitles = 20

// subtitleCodes are OpenSubtitles' own language codes.
var subtitleCodes = map[string]string{"pob": "por", "pb": "por", "scc": "srp", "zht": "zho", "zhe": "zho", "ze": "zho", "chi": "zho"}

// subtitleLanguage is an addon's language code ("eng", "ger", "en",
// OpenSubtitles' "pob") as ISO 639-2; "" when it isn't one.
func subtitleLanguage(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if c, ok := subtitleCodes[code]; ok {
		code = c
	}
	if len(code) < 2 {
		return ""
	}
	t, err := language.Parse(code)
	if err != nil {
		return ""
	}
	base, conf := t.Base()
	if conf == language.No {
		return ""
	}
	return base.ISO3()
}

// pickSubtitles keeps the subtitles in the wanted languages (ISO 639-1;
// none means English), up to per language, wanted languages first and each
// in the addons' order.
func pickSubtitles(offers []stremio.SubtitleOffer, want []string, per int) []SubtitleChoice {
	if len(want) == 0 {
		want = []string{"en"}
	}
	per = max(per, 1)
	var out []SubtitleChoice
	seen := map[string]bool{}
	for _, w := range want {
		lang := subtitleLanguage(w)
		n := 0
		for _, o := range offers {
			if n >= per || len(out) >= maxAddonSubtitles {
				break
			}
			if subtitleLanguage(o.Subtitle.Lang) != lang || seen[o.Subtitle.URL] {
				continue
			}
			seen[o.Subtitle.URL] = true
			out = append(out, SubtitleChoice{Language: lang, URL: o.Subtitle.URL, Addon: o.Addon})
			n++
		}
	}
	return out
}

func hasChoice(list []StreamChoice, id uuid.UUID) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

func streamChoice(item uuid.UUID, r stremio.Ranked) StreamChoice {
	s := r.Stream
	key, target := "url:"+s.URL, s.URL
	if s.InfoHash != "" {
		idx := -1
		if s.FileIdx != nil {
			idx = *s.FileIdx
		}
		target = resolve.Magnet(s.InfoHash, idx, s.Hints.Filename)
		key = "bt:" + target
	}
	return StreamChoice{
		ID: uuid.NewSHA1(item, []byte(key)), Name: streamLabel(r), Target: target,
		Container: streamContainer(s), Size: r.Info.Size,
	}
}

// streamContainer guesses the container from the file name or URL path.
func streamContainer(s stremio.Stream) string {
	name := s.Hints.Filename
	if name == "" && s.URL != "" {
		if u, err := url.Parse(s.URL); err == nil {
			name = u.Path
		}
	}
	switch ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), ".")); ext {
	case "mkv", "mp4", "webm", "avi", "ts", "m2ts", "mov":
		return ext
	case "m4v":
		return "mp4"
	}
	return ""
}

// streamLabel is a choice's name, e.g. "2160p DV HEVC Remux • 29.2 GB •
// Torrentio • cached": the picture, the size, the addon and, when known,
// whether a debrid account has the torrent.
func streamLabel(r stremio.Ranked) string {
	in := r.Info
	var pic []string
	if in.Height > 0 {
		pic = append(pic, strconv.Itoa(in.Height)+"p")
	}
	switch {
	case in.DV:
		pic = append(pic, "DV")
	case in.HDR:
		pic = append(pic, "HDR")
	}
	if c := codecLabels[in.Codec]; c != "" {
		pic = append(pic, c)
	}
	if in.Remux {
		pic = append(pic, "Remux")
	}
	if in.Cam {
		pic = append(pic, "CAM")
	}
	parts := []string{strings.Join(pic, " ")}
	if parts[0] == "" {
		parts[0] = "Stream"
	}
	if in.Size > 0 {
		parts = append(parts, fmt.Sprintf("%.1f GB", float64(in.Size)/(1<<30)))
	}
	if r.Addon != "" {
		parts = append(parts, r.Addon)
	}
	switch {
	case in.Debrid:
		parts = append(parts, "cached")
	case in.Uncached:
		parts = append(parts, "not cached")
	}
	return strings.Join(parts, " • ")
}

var codecLabels = map[string]string{"hevc": "HEVC", "av1": "AV1", "h264": "H.264", "vp9": "VP9"}

// streamPrefs reads what the ranking needs from the client's profile. A
// request without one (GET PlaybackInfo) is treated as able to play
// anything, so nothing is penalised on a guess.
func streamPrefs(p media.DeviceProfile, maxBitrate int64) stremio.Prefs {
	prefs := stremio.Prefs{MaxBitrate: maxBitrate}
	if prefs.MaxBitrate <= 0 {
		prefs.MaxBitrate = p.MaxStreamingBitrate
	}
	if len(p.DirectPlayProfiles) == 0 {
		prefs.HEVC, prefs.AV1, prefs.HDR = true, true, true
		return prefs
	}
	for _, dp := range p.DirectPlayProfiles {
		if !strings.EqualFold(dp.Type, "Video") {
			continue
		}
		prefs.HEVC = prefs.HEVC || dp.VideoCodec == "" || media.CodecIn(dp.VideoCodec, "hevc")
		prefs.AV1 = prefs.AV1 || dp.VideoCodec == "" || media.CodecIn(dp.VideoCodec, "av1")
	}
	prefs.HDR = true
	sawRange := false
	for _, cp := range p.CodecProfiles {
		if !strings.EqualFold(cp.Type, "Video") {
			continue
		}
		for _, c := range cp.Conditions {
			n, _ := strconv.Atoi(c.Value)
			switch {
			case strings.EqualFold(c.Property, "VideoRangeType"):
				// SDR-only clients list just "SDR"; HDR ones add HDR10, DOVI…
				if !sawRange {
					sawRange, prefs.HDR = true, false
				}
				v := strings.ToUpper(c.Value)
				prefs.HDR = prefs.HDR || strings.Contains(v, "HDR") || strings.Contains(v, "DOVI")
			case strings.EqualFold(c.Condition, "LessThanEqual") && strings.EqualFold(c.Property, "Height"):
				prefs.MaxHeight = minCap(prefs.MaxHeight, n)
			case strings.EqualFold(c.Condition, "LessThanEqual") && strings.EqualFold(c.Property, "Width"):
				prefs.MaxHeight = minCap(prefs.MaxHeight, heightForWidth(n))
			}
		}
	}
	return prefs
}

// minCap is the lower of two caps, where 0 means none.
func minCap(cur, n int) int {
	if n <= 0 {
		return cur
	}
	if cur == 0 || n < cur {
		return n
	}
	return cur
}

// heightForWidth maps a width cap onto the resolution it allows.
func heightForWidth(w int) int {
	switch {
	case w <= 0:
		return 0
	case w >= 3840:
		return 2160
	case w >= 1920:
		return 1080
	case w >= 1280:
		return 720
	}
	return 480
}
