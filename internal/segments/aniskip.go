package segments

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sysadmin/blockbustr/internal/cache"
)

// AniSkipURL is the community skip-times API anime players use.
const AniSkipURL = "https://api.aniskip.com"

const (
	aniSkipTimeout  = 3 * time.Second
	aniSkipParallel = 2 // requests in flight to aniskip.com
	aniSkipHit      = 7 * 24 * time.Hour
	aniSkipMiss     = 24 * time.Hour
	// aniSkipDrift is how far a submission's episode length may be from
	// the version's runtime: a release with another cold open or recap
	// shifts every time.
	aniSkipDrift = 3 * time.Second
	// aniSkipWarnEvery bounds the "AniSkip unreachable" log line.
	aniSkipWarnEvery = time.Minute
)

// AniSkip asks api.aniskip.com for an anime episode's skip times by its
// MyAnimeList id and episode number. Answers, misses and failures alike,
// are cached; a failure is no segments, never an error.
type AniSkip struct {
	// BaseURL is AniSkipURL unless set (tests).
	BaseURL string
	// HTTP is a client with a aniSkipTimeout timeout unless set.
	HTTP  *http.Client
	Cache *cache.Cache // nil: nothing cached
	Log   *slog.Logger

	once     sync.Once
	slots    chan struct{}
	mu       sync.Mutex
	lastWarn time.Time
}

// skipTime is one submission: what it skips and the episode length the
// submitter's file had.
type skipTime struct {
	Type          string  `json:"skipType"` // op, ed, mixed-op, mixed-ed, recap
	Start, End    float64 // seconds
	EpisodeLength float64 `json:"episodeLength"`
}

// Segments are the skip times of MAL anime malID's episode whose
// submissions were made against a file of this runtime (±aniSkipDrift).
// With no runtime known none qualify.
func (a *AniSkip) Segments(ctx context.Context, malID, episode int, runtime time.Duration) []Segment {
	if runtime <= 0 || malID <= 0 || episode <= 0 {
		return nil
	}
	var out []Segment
	seen := map[Type]bool{}
	for _, s := range a.times(ctx, malID, episode) {
		t := aniSkipType(s.Type)
		length := time.Duration(s.EpisodeLength * float64(time.Second))
		if t == "" || seen[t] || absDuration(length-runtime) > aniSkipDrift {
			continue
		}
		seg := Segment{Type: t, Start: seconds(s.Start), End: seconds(s.End)}
		if !sane(seg, runtime) {
			continue
		}
		seen[t] = true // the first (best voted) of each kind
		out = append(out, seg)
	}
	return out
}

func aniSkipType(s string) Type {
	switch s {
	case "op", "mixed-op":
		return Intro
	case "ed", "mixed-ed":
		return Outro
	case "recap":
		return Recap
	}
	return ""
}

func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func aniSkipKey(malID, episode int) cache.Key {
	return cache.Key("aniskip:" + strconv.Itoa(malID) + ":" + strconv.Itoa(episode))
}

// times are an episode's submissions, from the cache or AniSkip.
func (a *AniSkip) times(ctx context.Context, malID, episode int) []skipTime {
	key := aniSkipKey(malID, episode)
	if a.Cache != nil {
		var cached []skipTime
		if ok, err := a.Cache.GetJSON(ctx, key, &cached); err == nil && ok {
			return cached
		}
	}
	got, err := a.fetch(ctx, malID, episode)
	if err != nil {
		a.warn(ctx, err)
	}
	if a.Cache != nil && ctx.Err() == nil {
		ttl := aniSkipHit
		if len(got) == 0 {
			ttl = aniSkipMiss // none submitted, or AniSkip unreachable
		}
		if got == nil {
			got = []skipTime{}
		}
		_ = a.Cache.SetJSON(context.WithoutCancel(ctx), key, got, ttl)
	}
	return got
}

func (a *AniSkip) fetch(ctx context.Context, malID, episode int) ([]skipTime, error) {
	a.once.Do(func() { a.slots = make(chan struct{}, aniSkipParallel) })
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	base := a.BaseURL
	if base == "" {
		base = AniSkipURL
	}
	u := fmt.Sprintf("%s/v2/skip-times/%d/%d?types[]=op&types[]=ed&types[]=mixed-op&types[]=mixed-ed&types[]=recap&episodeLength=0", base, malID, episode)
	rctx, cancel := context.WithTimeout(ctx, aniSkipTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c := a.HTTP
	if c == nil {
		c = &http.Client{Timeout: aniSkipTimeout}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aniskip: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // nothing submitted for the episode
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aniskip: %s", resp.Status)
	}
	var body struct {
		Found   bool `json:"found"`
		Results []struct {
			Interval struct {
				StartTime float64 `json:"startTime"`
				EndTime   float64 `json:"endTime"`
			} `json:"interval"`
			SkipType      string  `json:"skipType"`
			EpisodeLength float64 `json:"episodeLength"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("aniskip: %w", err)
	}
	out := make([]skipTime, 0, len(body.Results))
	for _, r := range body.Results {
		out = append(out, skipTime{Type: r.SkipType, Start: r.Interval.StartTime, End: r.Interval.EndTime, EpisodeLength: r.EpisodeLength})
	}
	return out, nil
}

// warn logs AniSkip being unreachable, at most once per aniSkipWarnEvery.
func (a *AniSkip) warn(ctx context.Context, err error) {
	if a.Log == nil {
		return
	}
	a.mu.Lock()
	quiet := time.Since(a.lastWarn) < aniSkipWarnEvery
	if !quiet {
		a.lastWarn = time.Now()
	}
	a.mu.Unlock()
	if !quiet {
		a.Log.InfoContext(ctx, "AniSkip unreachable; anime episodes get no skip times from it for now", "err", err)
	}
}
