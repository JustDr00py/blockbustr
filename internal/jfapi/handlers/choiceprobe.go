package handlers

import (
	"context"
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

// choiceProbe is what a probe found, cached per choice (probe:choice:{id}).
type choiceProbe struct {
	Container    string
	Bitrate      int64
	RuntimeTicks int64
	Streams      []db.MediaStream // MediaSourceID set when attached
}

const (
	// probeWait bounds how long PlaybackInfo waits for probes it started;
	// the rest finish in the background for the next request.
	probeWait = 5 * time.Second
	// probeTimeout bounds one probe (resolve + ffprobe).
	probeTimeout = 45 * time.Second
	// probeParallel bounds probes across all requests. Probes mostly wait
	// on round trips (header reads through the addon's proxy), so many can
	// run at once: a title's versions finish in about the slowest's time.
	probeParallel = 8
)

func choiceProbeKey(id uuid.UUID) cache.Key { return cache.ProbeKey("choice:" + id.String()) }

// loadChoiceProbes returns the cached probes of choices.
func (a *api) loadChoiceProbes(ctx context.Context, choices []StreamChoice) map[uuid.UUID]choiceProbe {
	out := map[uuid.UUID]choiceProbe{}
	if a.Cache == nil {
		return out
	}
	for _, c := range choices {
		var p choiceProbe
		if ok, err := a.Cache.GetJSON(ctx, choiceProbeKey(c.ID), &p); err == nil && ok {
			out[c.ID] = p
		}
	}
	return out
}

// probeChoices probes the ready, unprobed choices in the background, the
// best-ranked first (it takes a slot first), and waits up to wait for them
// (0: doesn't wait). The channel closes when every probe it started ended.
func (a *api) probeChoices(ctx context.Context, choices []StreamChoice, wait time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if a.ChoiceProber == nil || a.Resolver == nil || a.Cache == nil {
		close(done)
		return done
	}
	have := a.loadChoiceProbes(ctx, choices)
	var todo []StreamChoice
	for _, c := range choices {
		if _, ok := have[c.ID]; !ok && c.Ready && !strings.HasPrefix(c.Target, "magnet:") {
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
				a.probeHeld(bg, c)
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

// probeChoice probes one choice, taking a slot first.
func (a *api) probeChoice(ctx context.Context, c StreamChoice) {
	select {
	case a.probeSem <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-a.probeSem }()
	a.probeHeld(ctx, c)
}

// probeHeld resolves and probes one choice (holding a slot) and caches the
// result; concurrent probes of one choice share the work.
func (a *api) probeHeld(ctx context.Context, c StreamChoice) {
	_, _, _ = a.probeFlight.Do(c.ID.String(), func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		start := time.Now()
		link, err := a.Resolver.Resolve(ctx, resolve.FromURL(c.Target))
		if err != nil {
			a.Log.DebugContext(ctx, "choice probe: not resolvable", "choice", c.ID, "err", err)
			return nil, err
		}
		info, err := a.ChoiceProber.Probe(ctx, link.URL, media.Options{Remote: true, SkipChapters: true})
		if err != nil {
			a.Log.InfoContext(ctx, "choice probe failed", "choice", c.ID, "err", err)
			return nil, err
		}
		p := choiceProbe{Container: probedContainer(info.Container), Bitrate: info.Bitrate, RuntimeTicks: int64(info.Duration / 100)}
		for _, st := range info.Streams {
			p.Streams = append(p.Streams, mediaStreamRow(st))
		}
		if err := a.Cache.SetJSON(ctx, choiceProbeKey(c.ID), p, cache.ProbeTTL); err != nil {
			return nil, err
		}
		a.Log.InfoContext(ctx, "probed stream choice", "choice", c.ID, "streams", len(p.Streams), "took", time.Since(start).Round(time.Millisecond))
		return nil, nil
	})
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
// preferred language's audio as the default.
func applyProbes(sources []db.MediaSource, streams map[uuid.UUID][]db.MediaStream, probes map[uuid.UUID]choiceProbe, lang string) {
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
		preferAudio(own, lang)
		streams[src.ID] = append(own, streams[src.ID]...)
	}
}

// preferAudio makes the first audio track in lang (ISO 639-2, "eng") the
// default when there is one: a multi-audio release often starts with
// another language.
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
		if streams[i].Type == "Audio" {
			streams[i].IsDefault = i == pick
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
