package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/metrics"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

// The playback log (admin UI's Playback page): a row per play, so admins
// can see per user what was played, which addon version, how it reached
// the player and how many bytes blockbustr proxied for it. The Playing
// report starts a row and Progress/Stopped keep it current (applyPlayback);
// a stream request adds what it served, meeting the report on the play
// session id. Stream requests without a play session (Findroid builds its
// own URL) can't be told apart by user and aren't logged. A new play
// session of a play still going on the same device (Void asks again on
// every track change) joins that play's row (logPlayback). A request with
// neither a play session nor a token (Infuse opens the details' signed
// Path and sends no headers) takes the item's play session when only one
// user recently asked for one (withItemPlaySession).
//
// GET /blockbustr/playback?userId=&before=<id>&limit=<n>&days=<n> returns
// the newest plays before the id and each user's totals over the days.

const (
	playbackLogFlush = 30 * time.Second // proxied bytes are added at least this often
	playbackLogPage  = 100
	playbackLogDays  = 30
	// itemPlayRecent is how long after PlaybackInfo a stream request with
	// neither a play session nor a token is taken to be that play's: a long
	// film with pauses and seeks.
	itemPlayRecent = 6 * time.Hour
)

// Deliveries: how a stream reached the player.
const (
	deliveryLocal      = "local"      // a file served by blockbustr
	deliveryProxied    = "proxied"    // a remote link fetched through blockbustr
	deliveryRedirected = "redirected" // the player was sent to the link
	deliveryRemuxed    = "remuxed"    // ffmpeg copies the video into HLS; its input download counts
	deliveryTranscoded = "transcoded" // ffmpeg re-encodes into HLS; its input download counts
)

// hlsDelivery is how an HLS session's output reaches the player.
func hlsDelivery(o transcode.StartOptions) string {
	if o.CopyVideo {
		return deliveryRemuxed
	}
	return deliveryTranscoded
}

// logPlayback updates the device's running play of the item
// (MergePlayback), or else starts or updates the play session's row, and
// returns the row's id.
func (a *api) logPlayback(ctx context.Context, p db.UpsertPlaybackParams) (int64, error) {
	id, err := a.Queries.MergePlayback(ctx, db.MergePlaybackParams{
		DeviceName: p.DeviceName, Client: p.Client, PlayMethod: p.PlayMethod, SourceName: p.SourceName,
		Addon: p.Addon, Delivery: p.Delivery, LinkHost: p.LinkHost, Reopen: p.Reopen,
		UserID: &p.UserID, ItemID: p.ItemID, DeviceID: p.DeviceID, PlaySessionID: p.PlaySessionID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	return a.Queries.UpsertPlayback(ctx, p)
}

// logPlayStart starts the play's row; 0 when it isn't logged.
func (a *api) logPlayStart(ctx context.Context, s auth.Session, rep playReport) int64 {
	id, err := a.logPlayback(ctx, db.UpsertPlaybackParams{
		UserID: s.UserID, DeviceID: s.DeviceID, DeviceName: s.DeviceName, Client: s.AppName, ItemID: rep.ItemID,
		PlaySessionID: nonEmpty(rep.PlaySessionID), PlayMethod: rep.PlayMethod, Reopen: true,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.Log.WarnContext(ctx, "playback log failed", "item", rep.ItemID, "err", err)
		}
		return 0
	}
	return id
}

// logPlayProgress updates a play's row from a Progress or Stopped report.
func (a *api) logPlayProgress(ctx context.Context, id int64, rep playReport, position int64, stopped bool) {
	if id == 0 {
		return
	}
	if rep.PositionTicks != nil {
		position = *rep.PositionTicks
	}
	if err := a.Queries.UpdatePlaybackProgress(ctx, db.UpdatePlaybackProgressParams{
		ID: id, PositionTicks: position, PlayMethod: rep.PlayMethod, Stopped: stopped,
	}); err != nil {
		a.Log.WarnContext(ctx, "playback log failed", "item", rep.ItemID, "err", err)
	}
}

// streamLog says where a stream's proxied bytes are logged: on the row its
// play session was logged on, or, when the request names none (Streamyfin's
// direct play sends no PlaySessionId), on the latest play of the item by
// the user its token names. The zero value logs nothing.
type streamLog struct {
	row int64 // the play's row (0: none)
	// owner is whose monthly data quota the bytes count against, whether
	// or not a logged play claims them (a download doesn't).
	owner      uuid.UUID
	user, item uuid.UUID
	// What a sessionless stream adds to the play it joins.
	delivery, source, addon, host string
}

// logStream records what a stream request served and how, and says where
// to count its proxied bytes.
func (a *api) logStream(r *http.Request, it db.Item, src db.MediaSource, delivery, link string) streamLog {
	ctx := r.Context()
	var host, addon string
	if u, err := url.Parse(link); err == nil {
		host = u.Hostname()
	}
	if _, _, catalog := stremioRef(it); catalog {
		if set, ok := a.loadStreamSet(ctx, it.ID); ok {
			for _, c := range set.All() {
				if c.ID == src.ID {
					addon = c.Addon
					break
				}
			}
		}
	}
	if r2, ok := a.withItemPlaySession(r, jfapi.QueryOf(r), it.ID); ok {
		r = r2
	}
	q := jfapi.QueryOf(r)
	id := q.Get("PlaySessionId")
	if ps, ok := a.loadPlaySession(ctx, id); ok && ps.UserID != uuid.Nil {
		p := db.UpsertPlaybackParams{
			UserID: ps.UserID, DeviceID: ps.DeviceID, ItemID: it.ID, PlaySessionID: &id, SourceName: src.Name, Delivery: delivery,
			LinkHost: host, Addon: addon,
		}
		// The device, from its session (stream requests carry no token).
		var cs clientSession
		if a.Cache != nil && ps.DeviceID != "" {
			if ok, err := a.Cache.GetJSON(ctx, cache.SessionKey(ps.DeviceID), &cs); err == nil && ok && cs.UserID == ps.UserID {
				p.DeviceName, p.Client = cs.DeviceName, cs.Client
			}
		}
		row, err := a.logPlayback(ctx, p)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				a.Log.WarnContext(ctx, "playback log failed", "item", it.ID, "err", err)
			}
			return streamLog{owner: ps.UserID}
		}
		return streamLog{row: row, owner: ps.UserID}
	}
	// No play session: the request's token, if it has one, names the user.
	info := jfapi.AuthFrom(ctx)
	sess, err := a.Auth.Resolve(ctx, info.Token)
	if err != nil {
		// Logged so a client whose plays go unattributed can be told apart.
		a.Log.DebugContext(ctx, "stream not attributed", append([]any{"item", it.ID, "playSession", id != "", "token", info.Token != ""},
			streamRequestAttrs(r, q)...)...)
		return streamLog{}
	}
	sl := streamLog{owner: sess.UserID, user: sess.UserID, item: it.ID, delivery: delivery, source: src.Name, addon: addon, host: host}
	sl.attach(ctx, a, 0)
	return sl
}

// rememberItemPlay notes user's latest play session of an item, for
// withItemPlaySession.
func (a *api) rememberItemPlay(ctx context.Context, item, user uuid.UUID, playSession string) {
	if a.Cache == nil || user == uuid.Nil {
		return
	}
	k := item.String()
	if err := a.Cache.SetJSON(ctx, cache.ItemPlayKey(k, user.String()), itemPlay{PlaySession: playSession, At: time.Now()}, cache.PlaySessionTTL); err != nil {
		a.Log.WarnContext(ctx, "remembering a play session failed", "item", item, "err", err)
		return
	}
	if err := a.Cache.AddToSet(ctx, cache.ItemPlaysKey(k), cache.PlaySessionTTL, user.String()); err != nil {
		a.Log.WarnContext(ctx, "remembering a play session failed", "item", item, "err", err)
	}
}

// itemPlay is a user's latest play session of an item (cache.ItemPlayKey).
type itemPlay struct {
	PlaySession string
	At          time.Time
}

// withItemPlaySession gives a stream request that has neither a play
// session nor a token the item's latest one, when exactly one user asked
// PlaybackInfo for the item in the last itemPlayRecent: with several, it
// would be a guess. ok is false when r is left as it is.
func (a *api) withItemPlaySession(r *http.Request, q jfapi.Query, item uuid.UUID) (*http.Request, bool) {
	ctx := r.Context()
	if a.Cache == nil || q.Get("PlaySessionId") != "" || jfapi.AuthFrom(ctx).Token != "" {
		return r, false
	}
	k := item.String()
	users, err := a.Cache.SetMembers(ctx, cache.ItemPlaysKey(k))
	if err != nil {
		return r, false
	}
	var found string
	for _, u := range users {
		var p itemPlay
		if ok, err := a.Cache.GetJSON(ctx, cache.ItemPlayKey(k, u), &p); err != nil || !ok || time.Since(p.At) > itemPlayRecent {
			continue
		}
		if found != "" {
			return r, false
		}
		found = p.PlaySession
	}
	if found == "" {
		return r, false
	}
	a.Log.DebugContext(ctx, "stream given the item's play session", append([]any{"item", item, "playSession", found},
		streamRequestAttrs(r, q)...)...)
	u := *r.URL
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += "PlaySessionId=" + url.QueryEscape(found)
	r = r.WithContext(ctx)
	r.URL = &u
	return r, true
}

// streamRequestAttrs describes how a stream request identifies itself, for
// debug logs: the names of its query parameters and auth headers (never
// their values), the token's source, the client and the user agent.
func streamRequestAttrs(r *http.Request, q jfapi.Query) []any {
	var headers []string
	for _, h := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
		if r.Header.Get(h) != "" {
			headers = append(headers, h)
		}
	}
	info := jfapi.AuthFrom(r.Context())
	return []any{"tokenSource", info.TokenSource, "client", info.Client, "query", q.Keys(), "authHeaders", headers, "userAgent", r.UserAgent()}
}

// attach adds bytes (and what the stream served) to the play it belongs to.
// False: no play of the item is logged yet, so the caller keeps the bytes
// for later, when the player's Playing report has started the row.
func (l streamLog) attach(ctx context.Context, a *api, bytes int64) bool {
	_, err := a.Queries.AttachPlaybackStream(ctx, db.AttachPlaybackStreamParams{
		UserID: &l.user, ItemID: l.item, Delivery: l.delivery, SourceName: l.source, Addon: l.addon, LinkHost: l.host, Bytes: bytes,
	})
	switch {
	case err == nil:
		return true
	case errors.Is(err, pgx.ErrNoRows):
		return false
	}
	a.Log.WarnContext(ctx, "playback log failed", "item", l.item, "err", err)
	return true // a database error: drop the bytes rather than retry forever
}

// countingWriter counts proxied bytes as they go (a stream can last hours),
// and adds them to the play's log row and the owner's quota usage every
// playbackLogFlush and when done (flush).
type countingWriter struct {
	w        io.Writer
	a        *api
	ctx      context.Context
	sl       streamLog // zero: not logged
	pending  int64     // not on the play's log row yet
	unbilled int64     // not in the owner's usage yet
	last     time.Time
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	metrics.ProxiedBytes.Add(float64(n))
	c.pending += int64(n)
	c.unbilled += int64(n)
	if time.Since(c.last) >= playbackLogFlush {
		c.flush()
	}
	return n, err
}

func (c *countingWriter) flush() {
	c.last = time.Now()
	// The client may have hung up already: the bytes still count.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if c.unbilled > 0 && c.sl.owner != uuid.Nil {
		n := c.unbilled
		c.unbilled = 0
		if err := c.a.Queries.AddUserBytes(ctx, db.AddUserBytesParams{
			UserID: c.sl.owner, Hour: time.Now().UTC().Truncate(time.Hour), Bytes: n,
		}); err != nil {
			c.a.Log.WarnContext(ctx, "counting a user's bytes failed", "user", c.sl.owner, "err", err)
		}
	}
	if c.pending == 0 || (c.sl.row == 0 && c.sl.user == uuid.Nil) {
		return
	}
	if c.sl.row == 0 {
		// Without a play session the row may not exist until the player's
		// Playing report arrives: keep the bytes until it does.
		if c.sl.attach(ctx, c.a, c.pending) {
			c.pending = 0
		}
		return
	}
	n := c.pending
	c.pending = 0
	if err := c.a.Queries.AddPlaybackBytes(ctx, db.AddPlaybackBytesParams{Bytes: n, ID: c.sl.row}); err != nil {
		c.a.Log.WarnContext(ctx, "playback log failed", "row", c.sl.row, "err", err)
	}
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// The admin endpoint.

type playbackEntry struct {
	ID                                     int64
	UserID                                 *uuid.UUID `json:",omitempty"`
	UserName, DeviceName, Client, ItemName string
	ItemID                                 uuid.UUID
	PlayMethod, SourceName, Addon          string
	Delivery, LinkHost                     string
	Bytes, PositionTicks                   int64
	RuntimeTicks                           *int64 `json:",omitempty"`
	StartedAt, LastSeenAt                  time.Time
	StoppedAt                              *time.Time `json:",omitempty"`
}

type playbackTotal struct {
	UserID     *uuid.UUID `json:",omitempty"`
	UserName   string
	Plays      int64
	Seconds    int64
	Bytes      int64
	LastPlayed time.Time
}

type playbackResponse struct {
	Entries []playbackEntry
	Totals  []playbackTotal
	Days    int
	More    bool // older entries exist (ask again with before = the last id)
}

func (a *api) registerPlaybackLog(rt *jfapi.Router) {
	rt.Get("/blockbustr/playback", a.requireAdmin(a.getPlaybackLog))
}

func (a *api) getPlaybackLog(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := jfapi.QueryOf(r)
	p := db.ListPlaybackLogParams{Lim: playbackLogPage + 1}
	if v := q.Get("userId"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			jfapi.WriteJSON(w, r, http.StatusBadRequest, map[string]string{"Error": "userId is not a user id"})
			return
		}
		p.UserID = &id
	}
	if n, ok := q.Int("limit"); ok && n > 0 && n <= 1000 {
		p.Lim = int32(n) + 1
	}
	p.BeforeID, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	days := playbackLogDays
	if n, ok := q.Int("days"); ok && n > 0 && n <= 3650 {
		days = n
	}
	rows, err := a.Queries.ListPlaybackLog(r.Context(), p)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	totals, err := a.Queries.PlaybackTotals(r.Context(), time.Now().AddDate(0, 0, -days))
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := playbackResponse{Entries: make([]playbackEntry, 0, len(rows)), Totals: make([]playbackTotal, 0, len(totals)), Days: days}
	if len(rows) == int(p.Lim) {
		rows, out.More = rows[:len(rows)-1], true
	}
	for _, e := range rows {
		out.Entries = append(out.Entries, playbackEntry{
			ID: e.ID, UserID: e.UserID, UserName: e.UserName, DeviceName: e.DeviceName, Client: e.Client,
			ItemID: e.ItemID, ItemName: e.ItemName, PlayMethod: e.PlayMethod, SourceName: e.SourceName, Addon: e.Addon,
			Delivery: e.Delivery, LinkHost: e.LinkHost, Bytes: e.Bytes, PositionTicks: e.PositionTicks,
			RuntimeTicks: e.RuntimeTicks, StartedAt: e.StartedAt, LastSeenAt: e.LastSeenAt, StoppedAt: e.StoppedAt,
		})
	}
	for _, t := range totals {
		out.Totals = append(out.Totals, playbackTotal(t))
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}
