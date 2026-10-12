package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/segments"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// /MediaSegments tells players what of an episode they can skip ("Skip
// Intro", "Skip Credits"). Chapter times belong to one file, and a catalog
// episode has several versions: its segments come from the version the
// user plays (sticky.go remembers it), else the one their last PlaybackInfo
// put first, else the best ranked. They're read from that version's
// chapter markers (cached with its probe); without usable ones, an anime
// episode's come from AniSkip, kept only when submitted against a file of
// the same length.

// SkipTimes gives an anime episode's skip times (segments.AniSkip).
type SkipTimes interface {
	Segments(ctx context.Context, malID, episode int, runtime time.Duration) []segments.Segment
}

// AnimeList maps a series episode to its MAL anime (segments.AnimeList).
type AnimeList interface {
	Lookup(imdb string, tmdb, season, episode int) (mal, malEpisode int, ok bool)
}

func (a *api) registerSegments(rt *jfapi.Router) {
	rt.Get("/MediaSegments/{itemId}", a.requireUser(a.mediaSegments))
}

func (a *api) mediaSegments(w http.ResponseWriter, r *http.Request, s auth.Session) {
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	it, found, err := a.visibleItem(r, s.UserID, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var segs []segments.Segment
	version := id.UUID()
	if found { // like Jellyfin, an unknown item has no segments, not a 404
		segs, version = a.itemSegments(r.Context(), s.UserID, it)
	}
	want := jfapi.QueryOf(r).List("includeSegmentTypes")
	items := []dto.MediaSegmentDto{}
	for _, sg := range segs {
		if len(want) > 0 && !slices.ContainsFunc(want, func(t string) bool { return strings.EqualFold(t, string(sg.Type)) }) {
			continue
		}
		// The same segment of the same version keeps its id across calls.
		segID := dto.IDFromUUID(uuid.NewSHA1(it.ID, []byte(version.String()+"|"+string(sg.Type)+"|"+strconv.FormatInt(int64(sg.Start), 10))))
		items = append(items, dto.MediaSegmentDto{
			Id: &segID, ItemId: ptr(dto.IDFromUUID(it.ID)), Type: ptr(dto.MediaSegmentType(sg.Type)),
			StartTicks: ptr(int64(sg.Start / 100)), EndTicks: ptr(int64(sg.End / 100)),
		})
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.MediaSegmentDtoQueryResult{
		Items: &items, StartIndex: ptr(int32(0)), TotalRecordCount: ptr(int32(len(items))),
	})
}

// itemSegments are it's skippable segments for user, and the version (a
// choice id; the item's own for a file) they were read from.
func (a *api) itemSegments(ctx context.Context, user uuid.UUID, it db.Item) ([]segments.Segment, uuid.UUID) {
	cfg := a.live().Segments
	var chapters []media.Chapter
	var runtime time.Duration
	if it.RuntimeTicks != nil {
		runtime = time.Duration(*it.RuntimeTicks) * 100
	}
	version := it.ID
	if _, _, catalog := stremioRef(it); catalog {
		if it.Type != "Episode" {
			return nil, version
		}
		c, ok := a.segmentVersion(ctx, user, it)
		if ok {
			version = c.ID
			p, probed := a.loadChoiceProbes(ctx, []StreamChoice{c})[c.ID]
			if probed && p.RuntimeTicks > 0 {
				runtime = time.Duration(p.RuntimeTicks) * 100
			}
			if probed && p.ChaptersRead {
				chapters = p.Chapters
			} else if c.Ready && a.ChoiceProber != nil && !a.probeFailed(ctx, c) {
				// Not probed for its chapters yet: the next call has them.
				go a.probeChoice(context.WithoutCancel(ctx), it.ID, c)
			}
		}
	} else if cfg.Chapters && a.Queries != nil {
		rows, err := a.Queries.ListChaptersForItems(ctx, []uuid.UUID{it.ID})
		if err != nil {
			a.Log.WarnContext(ctx, "chapters unavailable", "item", it.ID, "err", err)
		}
		for _, c := range rows {
			chapters = append(chapters, media.Chapter{Start: time.Duration(c.StartTicks) * 100, Title: c.Name})
		}
	}

	var segs []segments.Segment
	from := "none"
	if cfg.Chapters {
		if segs = segments.FromChapters(chapters, runtime); len(segs) > 0 {
			from = "chapters"
		}
	}
	if len(segs) == 0 && cfg.AniSkip && it.Type == "Episode" {
		if segs = a.aniSkipSegments(ctx, it, runtime); len(segs) > 0 {
			from = "aniskip"
		}
	}
	a.Log.DebugContext(ctx, "media segments", "item", it.ID, "version", version, "from", from, "segments", len(segs),
		"chapters", len(chapters), "runtime", runtime)
	return segs, version
}

// aniSkipSegments are an anime episode's AniSkip skip times for a version
// of runtime; none for a series not in the anime mappings.
func (a *api) aniSkipSegments(ctx context.Context, it db.Item, runtime time.Duration) []segments.Segment {
	if a.SkipTimes == nil || a.AnimeList == nil || a.Queries == nil || it.IndexNumber == nil || it.ParentIndexNumber == nil {
		return nil
	}
	seriesID, err := a.Queries.EpisodeSeries(ctx, it.ID)
	if err != nil {
		return nil
	}
	series, err := a.Queries.GetItem(ctx, seriesID)
	if err != nil {
		return nil
	}
	var ids map[string]string
	_ = json.Unmarshal(series.ProviderIds, &ids)
	tmdb, _ := strconv.Atoi(ids["Tmdb"])
	mal, episode, ok := a.AnimeList.Lookup(ids["Imdb"], tmdb, int(*it.ParentIndexNumber), int(*it.IndexNumber))
	if !ok {
		return nil
	}
	return a.SkipTimes.Segments(ctx, mal, episode, runtime)
}

// segmentVersion is the version of a catalog episode whose segments user
// gets: the one their play streams, else the first of their last
// PlaybackInfo, else the best ranked.
func (a *api) segmentVersion(ctx context.Context, user uuid.UUID, it db.Item) (StreamChoice, bool) {
	set, ok := a.loadStreamSet(ctx, it.ID)
	if !ok || a.Cache == nil {
		return StreamChoice{}, false
	}
	all := set.All()
	find := func(id uuid.UUID) (StreamChoice, bool) {
		i := slices.IndexFunc(all, func(c StreamChoice) bool { return c.ID == id })
		if i < 0 {
			return StreamChoice{}, false
		}
		return all[i], true
	}
	var played uuid.UUID
	if ok, err := a.Cache.GetJSON(ctx, playedChoiceKey(user, it.ID), &played); err == nil && ok {
		if c, ok := find(played); ok {
			return c, true
		}
	}
	var p itemPlay
	if ok, err := a.Cache.GetJSON(ctx, cache.ItemPlayKey(it.ID.String(), user.String()), &p); err == nil && ok {
		if ps, ok := a.loadPlaySession(ctx, p.PlaySession); ok && len(ps.Order) > 0 {
			if c, ok := find(ps.Order[0]); ok {
				return c, true
			}
		}
	}
	return set.Choices[0], true
}
