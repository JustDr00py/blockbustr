package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Addon stream choices are probed (TASKS P3.7 follow-up), as a .strm is at
// first play: a choice then carries its real tracks, so clients can switch
// audio and subtitles, its default audio is the user's language (multi-
// audio releases often start with another), and PlaybackInfo decides
// direct play or transcode like for any probed source. Only choices that
// are ready are probed ahead of play: resolving one still downloading (or a
// torrent) would start a debrid download.

// ChoiceProber probes a resolved stream (media.Prober).
type ChoiceProber interface {
	Probe(ctx context.Context, target string, opts media.Options) (*media.Info, error)
}

// choiceProbe is what a probe found, kept per choice in Postgres
// (choice_probes): a version's file doesn't change, and probes held only
// in Redis were all redone after a restart.
type choiceProbe struct {
	Container    string
	Bitrate      int64
	RuntimeTicks int64
	Streams      []db.MediaStream // MediaSourceID set when attached
	// Chapters are an episode's chapter markers, for its skippable
	// segments (mediasegments.go); ChaptersRead says they were asked for
	// (a probe from before, or of a movie, didn't).
	Chapters     []media.Chapter `json:",omitempty"`
	ChaptersRead bool            `json:",omitempty"`
}

const (
	// probeWait bounds how long PlaybackInfo waits for probes it started;
	// the rest finish in the background for the next request.
	probeWait = 5 * time.Second
	// probeTimeout bounds one probe (resolve + ffprobe).
	probeTimeout = 45 * time.Second
	// probeFailTTL is how long a choice whose probe failed isn't probed
	// again: details and PlaybackInfo each probe, so a failure would
	// otherwise be retried on every request.
	probeFailTTL = 10 * time.Minute
	// probeParallel bounds probes across all requests. Probes mostly wait
	// on round trips (header reads through the addon's proxy), so many can
	// run at once: a title's versions finish in about the slowest's time.
	probeParallel = 8
	// probeHostParallel bounds the resolves probes send one addon host at
	// once: AIOStreams answered 6 of 7 resolves fired together with 400.
	probeHostParallel = 2
	// refusedRetry is how long a probe waits before asking again after a
	// refusal that may pass (resolve.Passing).
	refusedRetry = 750 * time.Millisecond
	// probeTouchEvery is how often reading a probe marks it used; one not
	// used for a while is pruned (cmd/blockbustr: choiceProbeKeep).
	probeTouchEvery = 24 * time.Hour
)

// hostSlots bounds concurrent requests per host.
type hostSlots struct {
	mu    sync.Mutex
	slots map[string]chan struct{}
}

// acquire takes one of host's n slots, waiting for one; it returns the
// release, or false when ctx ends first.
func (h *hostSlots) acquire(ctx context.Context, host string, n int) (func(), bool) {
	h.mu.Lock()
	if h.slots == nil {
		h.slots = map[string]chan struct{}{}
	}
	ch, ok := h.slots[host]
	if !ok {
		ch = make(chan struct{}, n)
		h.slots[host] = ch
	}
	h.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, true
	case <-ctx.Done():
		return nil, false
	}
}

// targetHost is the host a choice's target is resolved through ("" for a
// magnet: a debrid account, not an addon host).
func targetHost(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Scheme == "magnet" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// choiceProbeFailKey marks a choice whose probe failed recently.
func choiceProbeFailKey(id uuid.UUID) cache.Key {
	return cache.ProbeKey("choicefail:" + id.String())
}

// probeFailed reports whether c's probe failed in the last probeFailTTL.
func (a *api) probeFailed(ctx context.Context, c StreamChoice) bool {
	if a.Cache == nil {
		return false
	}
	var failed bool
	ok, err := a.Cache.GetJSON(ctx, choiceProbeFailKey(c.ID), &failed)
	return err == nil && ok
}

// loadChoiceProbes returns the stored probes of choices, marking those
// not marked in probeTouchEvery as used: a version still offered keeps
// its probe.
func (a *api) loadChoiceProbes(ctx context.Context, choices []StreamChoice) map[uuid.UUID]choiceProbe {
	out := map[uuid.UUID]choiceProbe{}
	if a.Queries == nil || len(choices) == 0 {
		return out
	}
	ids := make([]uuid.UUID, len(choices))
	for i, c := range choices {
		ids[i] = c.ID
	}
	rows, err := a.Queries.GetChoiceProbes(ctx, ids)
	if err != nil {
		if ctx.Err() == nil {
			a.Log.WarnContext(ctx, "choice probes unavailable", "err", err)
		}
		return out
	}
	touchBefore := time.Now().Add(-probeTouchEvery)
	var stale []uuid.UUID
	for _, r := range rows {
		var p choiceProbe
		if err := json.Unmarshal(r.Probe, &p); err != nil {
			continue // probed again, and overwritten
		}
		out[r.ChoiceID] = p
		if r.UsedAt.Before(touchBefore) {
			stale = append(stale, r.ChoiceID)
		}
	}
	if len(stale) > 0 {
		if err := a.Queries.TouchChoiceProbes(ctx, db.TouchChoiceProbesParams{Ids: stale, Before: touchBefore}); err != nil && ctx.Err() == nil {
			a.Log.WarnContext(ctx, "marking choice probes used failed", "err", err)
		}
	}
	return out
}

// probeChoices probes the ready, unprobed choices of item in the background, the
// best-ranked first (it takes a slot first), and waits up to wait for them
// (0: doesn't wait). Only the first stremio.streams.probe_versions ready
// choices are probed (0: all), probed or not; callers put a choice the
// client picked first. The channel closes when every probe it started
// ended.
func (a *api) probeChoices(ctx context.Context, item uuid.UUID, choices []StreamChoice, wait time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if a.ChoiceProber == nil || a.Resolver == nil || a.Cache == nil || a.Queries == nil {
		close(done)
		return done
	}
	var ready []StreamChoice
	for _, c := range choices {
		if c.Ready && !strings.HasPrefix(c.Target, "magnet:") {
			ready = append(ready, c)
		}
	}
	if n := a.live().Stremio.Streams.ProbeVersions; n > 0 && len(ready) > n {
		ready = ready[:n]
	}
	have := a.loadChoiceProbes(ctx, ready)
	var todo []StreamChoice
	for _, c := range ready {
		if _, ok := have[c.ID]; !ok && !a.probeFailed(ctx, c) {
			todo = append(todo, c)
		}
	}
	if len(todo) == 0 {
		close(done)
		return done
	}
	bg := context.WithoutCancel(ctx)
	go func() {
		var wg sync.WaitGroup
		for _, c := range todo {
			a.probeSem <- struct{}{} // in rank order: the likely pick goes first
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-a.probeSem }()
				a.probeHeld(bg, item, c)
			}()
		}
		wg.Wait()
		close(done)
	}()
	if wait > 0 {
		select {
		case <-done:
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
	return done
}

// probeChoice probes one choice of item, taking a slot first.
func (a *api) probeChoice(ctx context.Context, item uuid.UUID, c StreamChoice) {
	select {
	case a.probeSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.probeSem }()
	a.probeHeld(ctx, item, c)
}

// probeHeld resolves and probes one choice of item (holding a slot) and
// stores the result; concurrent probes of one choice share the work, and
// one stored while this waited for its slot isn't redone. A choice that
// serves no video is marked bad, so it's no longer offered or fallen back
// to; any failure isn't retried for probeFailTTL.
func (a *api) probeHeld(ctx context.Context, item uuid.UUID, c StreamChoice) {
	if a.Queries == nil || a.Cache == nil {
		return
	}
	_, _, _ = a.probeFlight.Do(c.ID.String(), func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		start := time.Now()
		// An episode's chapters say where its intro and credits are; a
		// movie's aren't used, and skipping them saves a read.
		episode := a.isEpisode(ctx, item)
		// Details and PlaybackInfo both queue a title's versions: the
		// second queue's turn often comes after the first probed them
		// (2026-10-11: a version probed twice on play, 2 s each).
		if p, ok := a.loadChoiceProbes(ctx, []StreamChoice{c})[c.ID]; ok && (p.ChaptersRead || !episode) {
			return nil, nil
		}
		info, err := a.resolveAndProbe(ctx, c, episode)
		if err != nil {
			_ = a.Cache.SetJSON(context.WithoutCancel(ctx), choiceProbeFailKey(c.ID), true, probeFailTTL)
			if errors.Is(err, resolve.ErrNotVideo) {
				a.markBadChoice(context.WithoutCancel(ctx), item, c.ID)
				a.Log.WarnContext(ctx, "stream choice serves no video; no longer offered", "item", item, "choice", c.ID, "err", err)
			} else {
				a.Log.InfoContext(ctx, "choice probe failed", "item", item, "choice", c.ID, "err", err)
			}
			return nil, err
		}
		p := choiceProbe{Container: probedContainer(info.Container), Bitrate: info.Bitrate, RuntimeTicks: int64(info.Duration / 100),
			Chapters: info.Chapters, ChaptersRead: episode}
		for _, st := range info.Streams {
			p.Streams = append(p.Streams, mediaStreamRow(st))
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		if err := a.Queries.SaveChoiceProbe(ctx, db.SaveChoiceProbeParams{ChoiceID: c.ID, ItemID: item, Probe: raw}); err != nil {
			a.Log.WarnContext(ctx, "saving a choice probe failed", "choice", c.ID, "err", err)
			return nil, err
		}
		a.Log.InfoContext(ctx, "probed stream choice", "choice", c.ID, "streams", len(p.Streams), "took", time.Since(start).Round(time.Millisecond))
		return nil, nil
	})
}

// resolveAndProbe resolves c and probes its link. ffprobe finding no media
// in it is ErrNotVideo once a fresh link says the same: a cached link may
// have expired onto an error page.
func (a *api) resolveAndProbe(ctx context.Context, c StreamChoice, chapters bool) (*media.Info, error) {
	target := resolve.FromURL(c.Target)
	for fresh := false; ; fresh = true {
		link, err := a.probeResolve(ctx, c, target)
		if err != nil {
			return nil, err
		}
		info, err := a.ChoiceProber.Probe(ctx, link.URL, media.Options{Remote: true, SkipChapters: !chapters})
		if err == nil || !media.IsInvalidData(err) {
			return info, err
		}
		a.Resolver.Forget(ctx, target)
		if fresh {
			return nil, fmt.Errorf("%w: %w", resolve.ErrNotVideo, err)
		}
	}
}

// probeResolve resolves a choice for its probe, at most probeHostParallel
// at a time per addon host, asking once more after a refusal that may pass.
func (a *api) probeResolve(ctx context.Context, c StreamChoice, target resolve.Source) (resolve.Link, error) {
	try := func() (resolve.Link, error) {
		if host := targetHost(c.Target); host != "" {
			release, ok := a.probeHosts.acquire(ctx, host, probeHostParallel)
			if !ok {
				return resolve.Link{}, ctx.Err()
			}
			defer release()
		}
		return a.Resolver.Resolve(ctx, target)
	}
	link, err := try()
	if !resolve.Passing(err) {
		return link, err
	}
	select {
	case <-time.After(refusedRetry):
	case <-ctx.Done():
		return resolve.Link{}, err
	}
	return try()
}

// isEpisode reports whether item is an episode.
func (a *api) isEpisode(ctx context.Context, item uuid.UUID) bool {
	if a.Queries == nil {
		return false
	}
	it, err := a.Queries.GetItem(ctx, item)
	return err == nil && it.Type == "Episode"
}

// probedContainer maps ffprobe's format names ("matroska,webm",
// "mov,mp4,m4a,…") to the short names sources use.
func probedContainer(format string) string {
	switch first, _, _ := strings.Cut(format, ","); first {
	case "matroska":
		return "mkv"
	case "mov":
		return "mp4"
	case "mpegts":
		return "ts"
	case "avi", "webm":
		return first
	}
	return ""
}

// mediaStreamRow is a probed stream as source rows hold it.
func mediaStreamRow(st media.Stream) db.MediaStream {
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	optInt := func(n int) *int32 {
		if n == 0 {
			return nil
		}
		v := int32(n)
		return &v
	}
	optF := func(f float64) *float32 {
		if f <= 0 {
			return nil
		}
		v := float32(f)
		return &v
	}
	r := db.MediaStream{
		Idx: int32(st.Index), Type: string(st.Type), Codec: opt(st.Codec), Language: opt(st.Language), Title: opt(st.Title),
		Profile: opt(st.Profile), IsDefault: st.IsDefault, IsForced: st.IsForced, IsHearingImpaired: st.IsHearingImpaired,
		IsOriginal: st.IsOriginal, IsExternal: st.IsExternal, ExternalPath: opt(st.Path),
		Width: optInt(st.Width), Height: optInt(st.Height), Bitrate: optInt(int(min(st.Bitrate, 1<<31-1))),
		Channels: optInt(st.Channels), ChannelLayout: opt(st.ChannelLayout), SampleRate: optInt(st.SampleRate),
		VideoRange: opt(st.VideoRange), VideoRangeType: opt(st.VideoRangeType), PixelFormat: opt(st.PixelFormat),
		BitDepth: optInt(st.BitDepth), AspectRatio: opt(st.AspectRatio), IsInterlaced: st.IsInterlaced,
		ColorTransfer: opt(st.ColorTransfer), ColorPrimaries: opt(st.ColorPrimaries), ColorSpace: opt(st.ColorSpace),
		ColorRange: opt(st.ColorRange), TimeBase: opt(st.TimeBase),
		Level: optF(float64(st.Level)), AverageFrameRate: optF(st.AverageFrameRate), RealFrameRate: optF(st.RealFrameRate),
	}
	if st.DoVi != nil {
		r.DvProfile, r.DvLevel, r.DvBlCompatID = ptr(int32(st.DoVi.Profile)), ptr(int32(st.DoVi.Level)), ptr(int32(st.DoVi.BLSignalCompatibilityID))
		r.DvVersionMajor, r.DvVersionMinor = ptr(int32(st.DoVi.VersionMajor)), ptr(int32(st.DoVi.VersionMinor))
		r.DvRpuPresent, r.DvElPresent, r.DvBlPresent = ptr(st.DoVi.RPU), ptr(st.DoVi.EL), ptr(st.DoVi.BL)
	}
	return r
}

// applyProbes gives the sources of probed choices their container,
// bitrate, runtime and tracks (ahead of the addon subtitles), with the
// preferred language's audio as the default. Sources whose file starts in
// another language go last, keeping their order: a proxied direct play
// plays the file's own default tracks, whatever the menu says.
func applyProbes(sources []db.MediaSource, streams map[uuid.UUID][]db.MediaStream, probes map[uuid.UUID]choiceProbe, lang string) {
	foreign := map[uuid.UUID]bool{}
	for i := range sources {
		p, ok := probes[sources[i].ID]
		if !ok {
			continue
		}
		src := &sources[i]
		if p.Container != "" {
			src.Container = ptr(p.Container)
		}
		if p.Bitrate > 0 {
			src.Bitrate = ptr(int32(min(p.Bitrate, 1<<31-1)))
		}
		if p.RuntimeTicks > 0 {
			src.RuntimeTicks = ptr(p.RuntimeTicks)
		}
		own := make([]db.MediaStream, len(p.Streams))
		for j, st := range p.Streams {
			st.MediaSourceID = src.ID
			own[j] = st
		}
		if start := startingAudio(own); lang != "" && start != "" && start != "und" && start != lang {
			foreign[src.ID] = true
		}
		preferAudio(own, lang)
		streams[src.ID] = append(own, streams[src.ID]...)
	}
	if len(foreign) > 0 {
		slices.SortStableFunc(sources, func(a, b db.MediaSource) int {
			switch fa, fb := foreign[a.ID], foreign[b.ID]; {
			case fa == fb:
				return 0
			case fa:
				return 1
			}
			return -1
		})
	}
}

// startingAudio is the language (ISO 639-2) of the audio track a player
// starts with: the first flagged default, else the first; "" when unknown.
func startingAudio(streams []db.MediaStream) string {
	first := -1
	for i, st := range streams {
		if st.Type != "Audio" {
			continue
		}
		if st.IsDefault {
			first = i
			break
		}
		if first < 0 {
			first = i
		}
	}
	if first < 0 || streams[first].Language == nil {
		return ""
	}
	return subtitleLanguage(*streams[first].Language)
}

// preferAudio makes the first audio track in lang (ISO 639-2, "eng") the
// default when there is one: a multi-audio release often starts with
// another language. Subtitles then aren't default unless in lang (forced
// French subtitles under English audio would be wrong); forced flags stay.
func preferAudio(streams []db.MediaStream, lang string) {
	if lang == "" {
		return
	}
	pick := -1
	for i, st := range streams {
		if st.Type == "Audio" && st.Language != nil && subtitleLanguage(*st.Language) == lang {
			pick = i
			break
		}
	}
	if pick < 0 {
		return
	}
	for i := range streams {
		switch streams[i].Type {
		case "Audio":
			streams[i].IsDefault = i == pick
		case "Subtitle":
			if l := streams[i].Language; l == nil || subtitleLanguage(*l) != lang {
				streams[i].IsDefault = false
			}
		}
	}
}

// preferredLanguages are the audio languages wanted (ISO 639-1): the
// configured ones, else metadata.language's ("en-US" → en).
func (a *api) preferredLanguages() []string {
	if l := a.live().Stremio.Streams.Languages; len(l) > 0 {
		return l
	}
	base, _, _ := strings.Cut(a.Config.Metadata.Language, "-")
	if base = strings.ToLower(strings.TrimSpace(base)); len(base) == 2 {
		return []string{base}
	}
	return nil
}

// preferredAudio is the first preferred language as ISO 639-2 ("eng").
func (a *api) preferredAudio() string {
	if l := a.preferredLanguages(); len(l) > 0 {
		return subtitleLanguage(l[0])
	}
	return ""
}
