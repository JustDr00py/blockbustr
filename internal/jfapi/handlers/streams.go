package handlers

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/text/language"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/events"
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
	Height    int    // from the labels (2160, 1080…); 0 when unknown
	Addon     string `json:",omitempty"` // the addon that offered it, for the playback log
	// Duration is the addon's runtime label in seconds, for a title whose
	// metadata has none (its bitrate estimate needs one).
	Duration int64 `json:",omitempty"`
	// Ready: playable now (a direct URL, or cached on debrid), so it can be
	// probed ahead of play without starting a download.
	Ready bool
}

// SubtitleChoice is one addon subtitle offered as an external track.
type SubtitleChoice struct {
	Language string // ISO 639-2, as ffprobe reports embedded tracks
	URL      string
	Addon    string
}

// streamSet is what streamset:{item} holds: the latest pick's choices,
// then the previous pick's — a client still playing one of those (a
// quality change re-picks under different prefs) or pinning one by id
// finds it — and the title's subtitles. Picks older than that drop off,
// so the versions a title offers stay two picks deep however many
// devices and qualities ask for it.
type streamSet struct {
	Choices   []StreamChoice
	Earlier   []StreamChoice
	Subtitles []SubtitleChoice
}

// All is every choice still resolvable by id, the latest pick first.
func (s streamSet) All() []StreamChoice {
	out := append([]StreamChoice{}, s.Choices...)
	for _, c := range s.Earlier {
		if !hasChoice(out, c.ID) {
			out = append(out, c)
		}
	}
	return out
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

// addonIDs are the ids to ask addons for an item's streams and subtitles,
// best first. Most addons, and the services behind aggregators such as
// AIOStreams, look titles up by IMDb id, so that form comes first when the
// item has one (from its catalog or TMDB): "tt…" for a movie or series,
// "tt…:season:episode" for an episode. The catalog's own id ("tmdb:603",
// "kitsu:1") follows.
func (a *api) addonIDs(ctx context.Context, it db.Item) (typ string, ids []string, ok bool) {
	typ, native, ok := stremioRef(it)
	if !ok {
		return "", nil, false
	}
	if strings.HasPrefix(native, "tt") {
		return typ, []string{native}, true
	}
	imdb := ""
	switch it.Type {
	case "Movie", "Series":
		imdb = imdbOf(it)
	case "Episode":
		if it.ParentID != nil && it.IndexNumber != nil && it.ParentIndexNumber != nil {
			if seasons, err := a.Queries.GetItemsByIDs(ctx, []uuid.UUID{*it.ParentID}); err == nil && len(seasons) == 1 && seasons[0].ParentID != nil {
				if series, err := a.Queries.GetItemsByIDs(ctx, []uuid.UUID{*seasons[0].ParentID}); err == nil && len(series) == 1 {
					if s := imdbOf(series[0]); s != "" {
						imdb = fmt.Sprintf("%s:%d:%d", s, *it.ParentIndexNumber, *it.IndexNumber)
					}
				}
			}
		}
	}
	if imdb == "" {
		return typ, []string{native}, true
	}
	return typ, []string{imdb, native}, true
}

func imdbOf(it db.Item) string {
	var ids map[string]string
	if json.Unmarshal(it.ProviderIds, &ids) == nil && strings.HasPrefix(ids["Imdb"], "tt") {
		return ids["Imdb"]
	}
	return ""
}

// withinHeight is choices without those above maxHeight (0: no cap). A
// choice whose labels don't say its height stays.
func withinHeight(choices []StreamChoice, maxHeight int) []StreamChoice {
	if maxHeight <= 0 {
		return choices
	}
	return slices.DeleteFunc(slices.Clone(choices), func(c StreamChoice) bool { return c.Height > maxHeight })
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
				Language: ptr(sub.Language), IsExternal: true, ExternalPath: ptr(sub.URL), // untitled: apps show the language
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
			runtime := deref(it.RuntimeTicks)
			if runtime <= 0 && c.Duration > 0 {
				runtime = c.Duration * 10_000_000
				src.RuntimeTicks = ptr(runtime)
			}
			if runtime > 0 {
				// An estimate from the size; probing at first play replaces it.
				src.Bitrate = ptr(int32(min(c.Size*8*10_000_000/runtime, 1<<31-1)))
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
		// Every choice offered gets its probe, earlier ones too: a source
		// without tracks transcodes with software decode (no codec known).
		// Those above the user's height cap aren't theirs to see or play.
		all := withinHeight(set.All(), maxHeight(ctx))
		var streams map[uuid.UUID][]db.MediaStream
		b.sources[it.ID], streams = choiceSources(it, all, set.Subtitles)
		applyProbes(b.sources[it.ID], streams, a.loadChoiceProbes(ctx, all), a.preferredAudio())
		for id, st := range streams {
			b.streams[id] = st
		}
	}
	return nil
}

// detailsWait is how long a title's details wait for its versions the
// first time it's opened (AIOStreams answers a new title in 3.5–4 s); a
// slower collection finishes in the background and tells the clients.
const detailsWait = 5 * time.Second

// prefetch is a background collection a details request may wait on.
type prefetch struct {
	done chan struct{} // closed when the versions are remembered
	mu   sync.Mutex
	// finished, or notify: whoever comes second publishes ItemsUpdated, so
	// a details answer that missed the versions is followed by an event.
	finished, notify bool
}

// prefetchStreamChoices collects a title's streams in the background when
// its details are opened with no set remembered (TASKS P3.7 follow-up), so
// the version picker fills without playing first; the details request waits
// up to detailsWait for it (waitPrefetch). PlaybackInfo still re-picks with
// the client's profile when played. The lock keeps one collection per
// title (a client syncing a library opens many details) and throttles
// retries while an addon fails; the semaphore bounds them across titles.
// The versions are then probed in the background, and clients are told
// when their tracks are known.
func (a *api) prefetchStreamChoices(ctx context.Context, it db.Item) *prefetch {
	if a.Streams == nil || a.Cache == nil {
		return nil
	}
	if _, _, ok := stremioRef(it); !ok {
		return nil
	}
	if _, ok := a.loadStreamSet(ctx, it.ID); ok {
		return nil
	}
	select {
	case a.prefetch <- struct{}{}:
	default:
		return nil // enough collections in flight; the next details view retries
	}
	lock, ok, err := a.Cache.TryLock(ctx, cache.StreamPrefetchKey(it.ID.String()), cache.StreamPrefetchTTL)
	if err != nil || !ok {
		<-a.prefetch
		return nil
	}
	pf := &prefetch{done: make(chan struct{})}
	go func() {
		pctx := context.WithoutCancel(ctx) // the request may be answered already
		pick, err := a.pickStreams(pctx, it, streamPrefs(media.DeviceProfile{}, 0))
		// The slot and lock cover the collection only: probing has its own
		// bounds, and holding them through it made quickly opened titles
		// skip their collection.
		_ = lock.Unlock(pctx)
		<-a.prefetch
		pf.mu.Lock()
		pf.finished = true
		notify := pf.notify
		pf.mu.Unlock()
		close(pf.done)
		if err != nil {
			a.Log.WarnContext(pctx, "background stream collection failed", "item", it.ID, "err", err)
			return
		}
		if len(pick.Choices) == 0 {
			return
		}
		if notify {
			a.itemUpdated(pctx, it.ID) // the details answer went out without them
		}
		// Tracks ready by the time it's played; then tell the clients again.
		<-a.probeChoices(pctx, pick.Choices, 0)
		a.itemUpdated(pctx, it.ID)
	}()
	return pf
}

// waitPrefetch waits up to detailsWait for pf. It reports whether the
// versions are in; if not, the collection notifies clients when they are.
func (a *api) waitPrefetch(ctx context.Context, pf *prefetch) bool {
	if pf == nil {
		return false
	}
	t := time.NewTimer(detailsWait)
	defer t.Stop()
	select {
	case <-pf.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	pf.mu.Lock()
	defer pf.mu.Unlock()
	if pf.finished {
		return true
	}
	pf.notify = true
	return false
}

// itemUpdated tells clients (WebSocket LibraryChanged, ItemsUpdated) that
// an item changed, so apps that listen refresh it.
func (a *api) itemUpdated(ctx context.Context, item uuid.UUID) {
	if a.Events != nil {
		a.Events.Publish(ctx, events.Event{Kind: events.LibraryChanged, Updated: []uuid.UUID{item}})
	}
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
	typ, ids, ok := a.addonIDs(ctx, it)
	if !ok || a.Streams == nil {
		return streamSet{}, nil
	}
	var subs []stremio.SubtitleOffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		var err error
		if subs, err = a.Streams.Subtitles(ctx, typ, ids[0]); err != nil {
			a.Log.WarnContext(ctx, "subtitle collection failed", "item", it.ID, "err", err)
		}
	}()
	var offers []stremio.Offer
	var err error
	for _, id := range ids {
		if offers, err = a.Streams.Collect(ctx, typ, id); err != nil || len(offers) > 0 {
			break
		}
	}
	<-done
	if err != nil {
		return streamSet{}, err
	}
	cfg := a.live().Stremio.Streams
	prefs.Languages, prefs.Allow, prefs.Deny = a.preferredLanguages(), cfg.AllowGroups, cfg.DenyGroups
	if prefs.Runtime == 0 && it.RuntimeTicks != nil {
		prefs.Runtime = time.Duration(*it.RuntimeTicks) * 100 // ticks are 100 ns
	}
	ranked := stremio.Rank(offers, prefs)
	uhd, hd := max(cfg.UHDSlots, 0), max(cfg.HDSlots, 0)
	total := uhd + hd
	// A client that caps its height never sees past it: the slots it would
	// have wasted on 4K go to versions it can play.
	avail := make([]stremio.Ranked, 0, len(ranked))
	bad := a.badChoices(ctx, it.ID)
	for _, r := range ranked {
		if bad[streamChoice(it.ID, r, prefs.Runtime).ID] || (prefs.MaxHeight > 0 && r.Info.Height > prefs.MaxHeight) {
			continue
		}
		avail = append(avail, r)
	}
	// Quotas before rank: the best uhd_slots at 2160p and up, the best
	// hd_slots below it, so several 4K releases can't crowd 1080p out of
	// the picker. Slots a bucket can't fill spill to the best remaining.
	picked := make([]stremio.Ranked, 0, total)
	var rest []stremio.Ranked
	nUHD, nHD := 0, 0
	for _, r := range avail {
		switch {
		case r.Info.Height >= 2160 && nUHD < uhd:
			nUHD++
			picked = append(picked, r)
		case r.Info.Height < 2160 && nHD < hd:
			nHD++
			picked = append(picked, r)
		default:
			rest = append(rest, r)
		}
	}
	for _, r := range rest {
		if len(picked) == total {
			break
		}
		picked = append(picked, r)
	}
	// Best first, so the default (first) source is the ranked best.
	slices.SortStableFunc(picked, func(x, y stremio.Ranked) int { return cmp.Compare(y.Score, x.Score) })
	choices := make([]StreamChoice, 0, len(picked))
	for _, r := range picked {
		choices = append(choices, streamChoice(it.ID, r, prefs.Runtime))
	}
	pick := streamSet{Choices: choices, Subtitles: pickSubtitles(subs, a.Config.Stremio.Subtitles.Languages, a.Config.Stremio.Subtitles.PerLanguage)}
	if len(choices) == 0 || a.Cache == nil {
		return pick, nil
	}
	set, _ := a.loadStreamSet(ctx, it.ID)
	// The previous pick's choices stay one generation back for clients
	// still on them; the generation before that drops off, so the
	// remembered list stays bounded however many devices and qualities
	// pick this title.
	earlier := make([]StreamChoice, 0, len(set.Choices))
	for _, c := range set.Choices {
		if len(earlier) >= maxStreamSet-len(choices) {
			break
		}
		if !hasChoice(choices, c.ID) {
			earlier = append(earlier, c)
		}
	}
	if err := a.Cache.SetJSON(ctx, cache.StreamSetKey(it.ID.String()), streamSet{Choices: choices, Earlier: earlier, Subtitles: pick.Subtitles}, cache.StreamSetTTL); err != nil {
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

// streamChoice is r as a choice of item, whose runtime (0: unknown) turns
// its size into a bitrate for the label.
func streamChoice(item uuid.UUID, r stremio.Ranked, runtime time.Duration) StreamChoice {
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
		ID: uuid.NewSHA1(item, []byte(key)), Name: streamLabel(r, runtime), Target: target,
		Container: streamContainer(s), Size: r.Info.Size, Height: r.Info.Height, Ready: r.Info.Cached, Addon: r.Addon,
		Duration: int64(r.Info.Duration / time.Second),
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

// streamLabel is a choice's name, e.g. "2160p DV HEVC Remux • Atmos
// TrueHD 7.1 • 29.2 GB • ~26 Mbps • cached": the picture, the audio, the
// size and its average bitrate over runtime (the title's, else the addon's)
// and, when known, whether a debrid account has it.
func streamLabel(r stremio.Ranked, runtime time.Duration) string {
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
	parts := []string{strings.Join(pic, " ")}
	if parts[0] == "" {
		parts[0] = "Stream"
	}
	if in.Audio != "" {
		parts = append(parts, in.Audio)
	}
	if in.Size > 0 {
		parts = append(parts, fmt.Sprintf("%.1f GB", float64(in.Size)/(1<<30)))
	}
	if b := in.Bitrate(runtime); b > 0 {
		parts = append(parts, fmt.Sprintf("~%d Mbps", (b+500_000)/1_000_000))
	}
	// No addon name: users found "AIOStreams | name" noise in the picker.
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
