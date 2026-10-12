package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// A series keeps to the release a user watches it in: once a play of an
// episode runs stickyAfter, that version's release (its addon's binge
// group, its release group) is the series' for the user, and later
// episodes rank versions of it first (stremio.Prefs.Sticky) unless only
// another release is playable now. Picking another version and watching it
// as long moves the series to that release. Movies aren't kept to a release.

// stickyAfter is how long a play has to run (its position past where it
// started) for its release to stick.
const stickyAfter = 5 * time.Minute

// stickyAfterTicks is stickyAfter in Jellyfin ticks (100 ns).
const stickyAfterTicks = int64(stickyAfter / 100)

// playedChoiceKey remembers the version a user's play of an item streams,
// for a client reporting progress against the item id.
func playedChoiceKey(user, item uuid.UUID) cache.Key {
	return cache.Key("playedchoice:" + user.String() + ":" + item.String())
}

// seriesRelease is the release user keeps to in the series of it, when it
// is an episode; zero otherwise.
func (a *api) seriesRelease(ctx context.Context, user uuid.UUID, it db.Item) stremio.Release {
	if it.Type != "Episode" || user == uuid.Nil || a.Queries == nil {
		return stremio.Release{}
	}
	series, err := a.Queries.EpisodeSeries(ctx, it.ID)
	if err != nil {
		return stremio.Release{}
	}
	row, err := a.Queries.GetSeriesRelease(ctx, db.GetSeriesReleaseParams{UserID: user, SeriesID: series})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.Log.WarnContext(ctx, "series release unavailable", "item", it.ID, "err", err)
		}
		return stremio.Release{}
	}
	return stremio.Release{Group: row.ReleaseGroup, BingeGroup: row.BingeGroup}
}

// stickyFirst puts the first version of rel (by binge group, else release
// group) first, as Score's sticky boost does when ranking: ahead of a
// higher resolution, never of a version playable now when it isn't.
// choices are best first; they're returned as they are without a match.
func stickyFirst(choices []StreamChoice, rel stremio.Release) []StreamChoice {
	if len(choices) < 2 || rel == (stremio.Release{}) {
		return choices
	}
	match := -1
	if rel.BingeGroup != "" {
		match = slices.IndexFunc(choices, func(c StreamChoice) bool { return c.Release.BingeGroup == rel.BingeGroup })
	}
	if match < 0 && rel.Group != "" {
		match = slices.IndexFunc(choices, func(c StreamChoice) bool { return strings.EqualFold(c.Release.Group, rel.Group) })
	}
	if match <= 0 || (!choices[match].Ready && choices[0].Ready) {
		return choices
	}
	out := make([]StreamChoice, 0, len(choices))
	out = append(out, choices[match])
	out = append(out, choices[:match]...)
	return append(out, choices[match+1:]...)
}

// rememberPlayedChoice notes the version a user's play of an episode
// streams (playLink), for keepSeriesRelease.
func (a *api) rememberPlayedChoice(ctx context.Context, it db.Item, choice uuid.UUID) {
	user := viewerOf(ctx)
	if it.Type != "Episode" || user == uuid.Nil || a.Cache == nil {
		return
	}
	_ = a.Cache.SetJSON(ctx, playedChoiceKey(user, it.ID), choice, 12*time.Hour)
}

// keepSeriesRelease makes the release of the version a user played of an
// episode the series' for them: the version reported, or the one the play
// streams when the client reports the item itself.
func (a *api) keepSeriesRelease(ctx context.Context, user, item uuid.UUID, reported string) {
	if a.Queries == nil || a.Cache == nil {
		return
	}
	series, err := a.Queries.EpisodeSeries(ctx, item)
	if err != nil {
		return // not an episode
	}
	set, ok := a.loadStreamSet(ctx, item)
	if !ok {
		return
	}
	all := set.All()
	find := func(id uuid.UUID) (StreamChoice, bool) {
		i := slices.IndexFunc(all, func(c StreamChoice) bool { return c.ID == id })
		if i < 0 {
			return StreamChoice{}, false
		}
		return all[i], true
	}
	var c StreamChoice
	ok = false
	if id, err := dto.ParseID(reported); err == nil {
		c, ok = find(id.UUID())
	}
	if !ok {
		var played uuid.UUID
		if found, err := a.Cache.GetJSON(ctx, playedChoiceKey(user, item), &played); err != nil || !found {
			return
		}
		if c, ok = find(played); !ok {
			return
		}
	}
	if c.Release == (stremio.Release{}) {
		return
	}
	if err := a.Queries.SetSeriesRelease(ctx, db.SetSeriesReleaseParams{
		UserID: user, SeriesID: series, ReleaseGroup: c.Release.Group, BingeGroup: c.Release.BingeGroup,
	}); err != nil {
		a.Log.WarnContext(ctx, "series release not kept", "item", item, "err", err)
		return
	}
	a.Log.DebugContext(ctx, "series release kept", "user", user, "series", series, "group", c.Release.Group, "binge_group", c.Release.BingeGroup)
}
