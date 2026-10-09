package handlers

import (
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/events"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Sessions and playback reporting (TASKS P2.9, DESIGN §8.4). A session is
// one device's connection, kept in Redis (sess:{deviceId}, 10 min sliding,
// listed in the sessions set) and refreshed by any authenticated request.
// Playing/Progress/Stopped update what it's playing and save the user's
// resume point and played state with Jellyfin's rules (resumePoint). Every
// reporting endpoint answers 204, even for unknown items: clients ignore
// the answer and some crash on errors.

const (
	activityThrottle = 30 * time.Second // Redis writes per device for plain activity
	progressSave     = 30 * time.Second // user_data writes per play while it runs
)

// Jellyfin's resume rules (server defaults).
const (
	minResumePct      = 5
	maxResumePct      = 90
	minResumeDuration = 300 * 10_000_000 // ticks: shorter items get no resume point
)

// nowPlaying is the play state of a session.
type nowPlaying struct {
	ItemID              uuid.UUID
	MediaSourceID       string
	PlaySessionID       string
	PositionTicks       int64
	IsPaused            bool
	IsMuted             bool
	CanSeek             bool
	VolumeLevel         *int32
	AudioStreamIndex    *int32
	SubtitleStreamIndex *int32
	PlayMethod          string
	RepeatMode          string
	PlaybackOrder       string
	LastSaved           time.Time
	LogID               int64 `json:",omitempty"` // its playback log row; 0: not logged
}

// clientSession is a device's session as stored in Redis.
type clientSession struct {
	ID                           string
	UserID                       uuid.UUID
	UserName                     string
	Client                       string
	DeviceID                     string
	DeviceName                   string
	AppVersion                   string
	RemoteEndPoint               string
	LastActivity                 time.Time
	LastPlaybackCheckIn          time.Time
	PlayableMediaTypes           []string
	SupportedCommands            []string
	SupportsMediaControl         bool
	SupportsPersistentIdentifier bool
	AppStoreURL, IconURL         string    // from the client's capabilities, echoed back
	LastPaused                   time.Time // last time playback paused
	NowPlaying                   *nowPlaying
}

func remoteEndPoint(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loadSession returns the device's session, a fresh one if there's none.
func (a *api) loadSession(ctx context.Context, r *http.Request, s auth.Session) clientSession {
	var cs clientSession
	if ok, err := a.Cache.GetJSON(ctx, cache.SessionKey(s.DeviceID), &cs); err != nil || !ok || cs.UserID != s.UserID {
		cs = clientSession{
			ID:                           dto.IDFromUUID(uuid.NewSHA1(a.ServerID.UUID(), []byte(s.UserID.String()+"|"+s.DeviceID))).String(),
			PlayableMediaTypes:           []string{},
			SupportedCommands:            []string{},
			SupportsPersistentIdentifier: true,
		}
	}
	cs.UserID, cs.UserName, cs.Client, cs.DeviceID, cs.DeviceName, cs.AppVersion = s.UserID, s.UserName, s.AppName, s.DeviceID, s.DeviceName, s.AppVersion
	cs.RemoteEndPoint, cs.LastActivity = remoteEndPoint(r), time.Now()
	return cs
}

func (a *api) saveSession(ctx context.Context, cs clientSession) error {
	if err := a.Cache.SetJSON(ctx, cache.SessionKey(cs.DeviceID), cs, cache.SessionTTL); err != nil {
		return err
	}
	a.lastTouch.Store(cs.DeviceID, cs.LastActivity)
	return a.Cache.AddToSet(ctx, cache.SessionsKey, cache.SessionTTL, cs.DeviceID)
}

// touchSession records activity for the request's device, at most every
// activityThrottle (called by requireUser).
func (a *api) touchSession(r *http.Request, s auth.Session) {
	if a.Cache == nil || s.DeviceID == "" {
		return
	}
	if last, ok := a.lastTouch.Load(s.DeviceID); ok && time.Since(last.(time.Time)) < activityThrottle {
		return
	}
	cs := a.loadSession(r.Context(), r, s)
	if err := a.saveSession(r.Context(), cs); err != nil {
		a.Log.WarnContext(r.Context(), "session update failed", "device", s.DeviceID, "err", err)
	}
}

// updateSession loads, changes and stores the device's session.
func (a *api) updateSession(r *http.Request, s auth.Session, change func(*clientSession)) {
	if a.Cache == nil || s.DeviceID == "" {
		return
	}
	cs := a.loadSession(r.Context(), r, s)
	change(&cs)
	if err := a.saveSession(r.Context(), cs); err != nil {
		a.Log.WarnContext(r.Context(), "session update failed", "device", s.DeviceID, "err", err)
	}
}

func (a *api) registerSessions(rt *jfapi.Router) {
	rt.Get("/Sessions", a.requireUser(a.listSessions))
	rt.Post("/Sessions/Capabilities", a.requireUser(a.capabilities))
	rt.Post("/Sessions/Capabilities/Full", a.requireUser(a.capabilitiesFull))
	rt.Post("/Sessions/Playing", a.requireUser(a.reportPlayback(playStart)))
	rt.Post("/Sessions/Playing/Progress", a.requireUser(a.reportPlayback(playProgress)))
	rt.Post("/Sessions/Playing/Stopped", a.requireUser(a.reportPlayback(playStop)))
	rt.Post("/Sessions/Playing/Ping", a.requireUser(a.pingPlayback))
	// Legacy query-string forms (Kodi and older clients).
	for _, p := range []string{"/PlayingItems/{itemId}", "/Users/{userId}/PlayingItems/{itemId}"} {
		rt.Post(p, a.requireUser(a.legacyPlayback(playStart)))
		rt.Post(p+"/Progress", a.requireUser(a.legacyPlayback(playProgress)))
		rt.Delete(p, a.requireUser(a.legacyPlayback(playStop)))
	}
}

func noContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

func (a *api) capabilitiesFull(w http.ResponseWriter, r *http.Request, s auth.Session) {
	var c dto.ClientCapabilitiesDto
	_ = jfapi.DecodeJSON(w, r, &c) // a bad body still answers 204
	a.updateSession(r, s, func(cs *clientSession) {
		cs.PlayableMediaTypes, cs.SupportedCommands = []string{}, []string{}
		if c.PlayableMediaTypes != nil {
			for _, m := range *c.PlayableMediaTypes {
				cs.PlayableMediaTypes = append(cs.PlayableMediaTypes, string(m))
			}
		}
		if c.SupportedCommands != nil {
			for _, m := range *c.SupportedCommands {
				cs.SupportedCommands = append(cs.SupportedCommands, string(m))
			}
		}
		cs.SupportsMediaControl = c.SupportsMediaControl != nil && *c.SupportsMediaControl
		cs.SupportsPersistentIdentifier = c.SupportsPersistentIdentifier == nil || *c.SupportsPersistentIdentifier
		cs.AppStoreURL, cs.IconURL = deref(c.AppStoreUrl), deref(c.IconUrl)
	})
	noContent(w)
}

func (a *api) capabilities(w http.ResponseWriter, r *http.Request, s auth.Session) {
	q := jfapi.QueryOf(r)
	a.updateSession(r, s, func(cs *clientSession) {
		cs.PlayableMediaTypes, cs.SupportedCommands = append([]string{}, q.List("playableMediaTypes")...), append([]string{}, q.List("supportedCommands")...)
		cs.SupportsMediaControl, _ = q.Bool("supportsMediaControl")
		if v, ok := q.Bool("supportsPersistentIdentifier"); ok {
			cs.SupportsPersistentIdentifier = v
		}
	})
	noContent(w)
}

type playEvent int

const (
	playStart playEvent = iota
	playProgress
	playStop
)

// playReport is a Playing/Progress/Stopped report, from a body or (legacy)
// the query string.
type playReport struct {
	ItemID              uuid.UUID
	MediaSourceID       string
	PlaySessionID       string
	PositionTicks       *int64
	IsPaused, IsMuted   *bool
	CanSeek             *bool
	VolumeLevel         *int32
	AudioStreamIndex    *int32
	SubtitleStreamIndex *int32
	PlayMethod          string
	RepeatMode          string
	PlaybackOrder       string
	Failed              bool
}

func (a *api) reportPlayback(ev playEvent) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		var b dto.PlaybackProgressInfo // Start bodies have the same fields
		var stop dto.PlaybackStopInfo
		var err error
		if ev == playStop {
			err = jfapi.DecodeJSON(w, r, &stop)
			b = dto.PlaybackProgressInfo{ItemId: stop.ItemId, MediaSourceId: stop.MediaSourceId, PlaySessionId: stop.PlaySessionId, PositionTicks: stop.PositionTicks}
		} else {
			err = jfapi.DecodeJSON(w, r, &b)
		}
		if err != nil || b.ItemId == nil {
			noContent(w)
			return
		}
		rep := playReport{
			ItemID: b.ItemId.UUID(), MediaSourceID: deref(b.MediaSourceId), PlaySessionID: deref(b.PlaySessionId),
			PositionTicks: b.PositionTicks, IsPaused: b.IsPaused, IsMuted: b.IsMuted, CanSeek: b.CanSeek, VolumeLevel: b.VolumeLevel,
			AudioStreamIndex: b.AudioStreamIndex, SubtitleStreamIndex: b.SubtitleStreamIndex,
			PlayMethod: string(deref(b.PlayMethod)), RepeatMode: string(deref(b.RepeatMode)), PlaybackOrder: string(deref(b.PlaybackOrder)),
			Failed: stop.Failed != nil && *stop.Failed,
		}
		a.applyPlayback(r, s, ev, rep)
		noContent(w)
	}
}

func (a *api) legacyPlayback(ev playEvent) sessionHandler {
	return func(w http.ResponseWriter, r *http.Request, s auth.Session) {
		id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
		if err != nil {
			noContent(w)
			return
		}
		q := jfapi.QueryOf(r)
		rep := playReport{ItemID: id.UUID(), MediaSourceID: q.Get("mediaSourceId"), PlaySessionID: q.Get("playSessionId"),
			PlayMethod: q.Get("playMethod"), RepeatMode: q.Get("repeatMode")}
		if v, err := strconv.ParseInt(q.Get("positionTicks"), 10, 64); err == nil {
			rep.PositionTicks = &v
		}
		if v, ok := q.Bool("isPaused"); ok {
			rep.IsPaused = &v
		}
		if v, ok := q.Bool("isMuted"); ok {
			rep.IsMuted = &v
		}
		for name, dst := range map[string]**int32{"audioStreamIndex": &rep.AudioStreamIndex, "subtitleStreamIndex": &rep.SubtitleStreamIndex, "volumeLevel": &rep.VolumeLevel} {
			if n, ok := q.Int(name); ok {
				*dst = ptr(int32(n))
			}
		}
		a.applyPlayback(r, s, ev, rep)
		noContent(w)
	}
}

// applyPlayback updates the session and saves the user's play state: on a
// stop, on pause/resume and track changes, and every progressSave.
func (a *api) applyPlayback(r *http.Request, s auth.Session, ev playEvent, rep playReport) {
	var save bool
	var prev *nowPlaying
	a.updateSession(r, s, func(cs *clientSession) {
		prev = cs.NowPlaying
		np := cs.NowPlaying
		if np == nil || np.ItemID != rep.ItemID || ev == playStart {
			np = &nowPlaying{ItemID: rep.ItemID, PlayMethod: "DirectPlay", RepeatMode: "RepeatNone", PlaybackOrder: "Default", CanSeek: true, LastSaved: time.Now()}
			// A progress or stop for a play the session doesn't know (it
			// expired, or the start report never came) is logged too.
			np.LogID = a.logPlayStart(r.Context(), s, rep)
		}
		changed := false
		if rep.PositionTicks != nil {
			np.PositionTicks = *rep.PositionTicks
		}
		if rep.IsPaused != nil {
			changed = changed || np.IsPaused != *rep.IsPaused
			if *rep.IsPaused && !np.IsPaused {
				cs.LastPaused = time.Now()
			}
			np.IsPaused = *rep.IsPaused
		}
		if rep.IsMuted != nil {
			np.IsMuted = *rep.IsMuted
		}
		if rep.CanSeek != nil {
			np.CanSeek = *rep.CanSeek
		}
		if rep.VolumeLevel != nil {
			np.VolumeLevel = rep.VolumeLevel
		}
		if rep.AudioStreamIndex != nil {
			changed = changed || np.AudioStreamIndex == nil || *np.AudioStreamIndex != *rep.AudioStreamIndex
			np.AudioStreamIndex = rep.AudioStreamIndex
		}
		if rep.SubtitleStreamIndex != nil {
			changed = changed || np.SubtitleStreamIndex == nil || *np.SubtitleStreamIndex != *rep.SubtitleStreamIndex
			np.SubtitleStreamIndex = rep.SubtitleStreamIndex
		}
		for dst, v := range map[*string]string{&np.MediaSourceID: rep.MediaSourceID, &np.PlaySessionID: rep.PlaySessionID,
			&np.PlayMethod: rep.PlayMethod, &np.RepeatMode: rep.RepeatMode, &np.PlaybackOrder: rep.PlaybackOrder} {
			if v != "" {
				*dst = v
			}
		}
		cs.LastPlaybackCheckIn = time.Now()
		switch ev {
		case playStop:
			save = !rep.Failed
			cs.NowPlaying = nil
			a.logPlayProgress(r.Context(), np.LogID, rep, np.PositionTicks, true)
		case playProgress:
			save = changed || time.Since(np.LastSaved) >= progressSave
			if save {
				np.LastSaved = time.Now()
				a.logPlayProgress(r.Context(), np.LogID, rep, np.PositionTicks, false)
			}
			cs.NowPlaying = np
		default:
			cs.NowPlaying = np
		}
	})
	if a.Transcoding != nil {
		ps := rep.PlaySessionID
		if ps == "" && prev != nil {
			ps = prev.PlaySessionID
		}
		switch {
		case ps == "":
		case ev == playStop:
			a.Transcoding.Sessions.Close(ps)
		default:
			// A player still reporting keeps its transcode, though it may
			// fetch no segment for minutes: Streamyfin buffers ~5 minutes,
			// and the throttle holds ffmpeg meanwhile. Without this the
			// idle reaper (1 min without a segment) closed it, and the next
			// segment restarted ffmpeg from scratch.
			a.Transcoding.Sessions.Get(ps) // marks it used
		}
	}
	if save {
		a.savePlayState(r, s.UserID, rep)
	}
}

// resumePoint applies Jellyfin's resume rules to a stop or progress at
// position: past maxResumePct the item is played (position reset); before
// minResumePct, or in an item shorter than minResumeDuration, no resume
// point is kept. touch says whether this counts as a play (last played).
func resumePoint(position, runtime int64) (pos int64, finished, touch bool) {
	if runtime <= 0 {
		return max(position, 0), false, position > 0
	}
	pct := position * 100 / runtime
	switch {
	case pct >= maxResumePct:
		return 0, true, true
	case runtime < minResumeDuration:
		return 0, false, position > 0
	case pct < minResumePct:
		return 0, false, false
	}
	return position, false, true
}

func (a *api) savePlayState(r *http.Request, user uuid.UUID, rep playReport) {
	if rep.PositionTicks == nil {
		return
	}
	it, found, err := a.visibleItem(r, user, rep.ItemID)
	if err != nil || !found || (it.Type != "Movie" && it.Type != "Episode") {
		return
	}
	pos, finished, touch := resumePoint(*rep.PositionTicks, deref(it.RuntimeTicks))
	if err := a.Queries.RecordPlayback(r.Context(), db.RecordPlaybackParams{
		UserID: user, ItemID: it.ID, PositionTicks: pos, Finished: finished, Touch: touch,
		AudioStreamIdx: rep.AudioStreamIndex, SubtitleStreamIdx: rep.SubtitleStreamIndex,
	}); err != nil {
		a.Log.WarnContext(r.Context(), "saving play state failed", "item", it.ID, "err", err)
		return
	}
	a.Events.Publish(r.Context(), events.Event{Kind: events.UserDataChanged, UserID: user, ItemIDs: []uuid.UUID{it.ID}})
}

// pingPlayback keeps a play session (and its transcode) alive.
func (a *api) pingPlayback(w http.ResponseWriter, r *http.Request, s auth.Session) {
	if ps := jfapi.QueryOf(r).Get("playSessionId"); ps != "" && a.Transcoding != nil {
		a.Transcoding.Sessions.Get(ps) // marks it used
	}
	a.updateSession(r, s, func(cs *clientSession) { cs.LastPlaybackCheckIn = time.Now() })
	noContent(w)
}

// nowPlayingFields are the ItemFields Jellyfin fills on NowPlayingItem.
var nowPlayingFields = []string{
	"chapters", "datecreated", "enablemediasourcedisplay", "externalurls", "genres", "width", "height", "ishd",
	"localtrailercount", "mediastreams", "originaltitle", "overview", "parentid", "path", "primaryimageaspectratio",
	"providerids", "specialfeaturecount", "studios", "taglines", "trickplay",
}

// listSessions serves GET /Sessions: active sessions (activeWithinSeconds),
// the caller's own unless admin, most recent first. Streamyfin polls it
// every ~2s, so it reads Redis and only loads now-playing items.
func (a *api) listSessions(w http.ResponseWriter, r *http.Request, s auth.Session) {
	out := []dto.SessionInfoDto{}
	if a.Cache == nil {
		jfapi.WriteJSON(w, r, http.StatusOK, out)
		return
	}
	q := jfapi.QueryOf(r)
	devices, err := a.Cache.SetMembers(r.Context(), cache.SessionsKey)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var since time.Time
	if n, ok := q.Int("activeWithinSeconds"); ok && n > 0 {
		since = time.Now().Add(-time.Duration(n) * time.Second)
	}
	var list []clientSession
	var stale []string
	for _, dev := range devices {
		var cs clientSession
		ok, err := a.Cache.GetJSON(r.Context(), cache.SessionKey(dev), &cs)
		switch {
		case err != nil:
			continue
		case !ok:
			stale = append(stale, dev)
			continue
		case (!s.IsAdmin && cs.UserID != s.UserID) || cs.LastActivity.Before(since):
			continue
		case q.Get("deviceId") != "" && q.Get("deviceId") != dev:
			continue
		}
		list = append(list, cs)
	}
	if len(stale) > 0 {
		_ = a.Cache.RemoveFromSet(r.Context(), cache.SessionsKey, stale...)
	}
	slices.SortFunc(list, func(x, y clientSession) int { return y.LastActivity.Compare(x.LastActivity) })

	var playing []uuid.UUID
	for _, cs := range list {
		if cs.NowPlaying != nil {
			playing = append(playing, cs.NowPlaying.ItemID)
		}
	}
	items := map[uuid.UUID]dto.BaseItemDto{}
	if len(playing) > 0 {
		rows, err := a.itemsInIDOrder(r.Context(), playing)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		o := dtoOptions{fields: map[string]bool{}, enableImages: true, imageTypeLimit: -1}
		for _, f := range nowPlayingFields {
			o.fields[f] = true
		}
		dtos, err := a.itemDtos(r.Context(), nil, rows, o)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		for i, row := range rows {
			items[row.ID] = dtos[i]
		}
	}
	for _, cs := range list {
		out = append(out, a.sessionDto(cs, items))
	}
	jfapi.WriteJSON(w, r, http.StatusOK, out)
}

func (a *api) sessionDto(cs clientSession, items map[uuid.UUID]dto.BaseItemDto) dto.SessionInfoDto {
	types := make([]dto.MediaType, len(cs.PlayableMediaTypes))
	for i, t := range cs.PlayableMediaTypes {
		types[i] = dto.MediaType(t)
	}
	cmds := make([]dto.GeneralCommandType, len(cs.SupportedCommands))
	for i, c := range cs.SupportedCommands {
		cmds[i] = dto.GeneralCommandType(c)
	}
	state := dto.PlayerStateInfo{CanSeek: ptr(false), IsPaused: ptr(false), IsMuted: ptr(false),
		RepeatMode: ptr(dto.RepeatMode("RepeatNone")), PlaybackOrder: ptr(dto.PlaybackOrder("Default"))}
	userID := dto.IDFromUUID(cs.UserID)
	d := dto.SessionInfoDto{
		PlayState: &state, AdditionalUsers: &[]dto.SessionUserInfo{}, RemoteEndPoint: ptr(cs.RemoteEndPoint),
		PlayableMediaTypes: &types, Id: ptr(cs.ID), UserId: &userID, UserName: ptr(cs.UserName), Client: ptr(cs.Client),
		LastActivityDate: ptr(dto.NewTime(cs.LastActivity)), LastPlaybackCheckIn: &dto.Time{},
		DeviceName: ptr(cs.DeviceName), DeviceId: ptr(cs.DeviceID), ApplicationVersion: ptr(cs.AppVersion), IsActive: ptr(true),
		SupportsMediaControl: ptr(cs.SupportsMediaControl), SupportsRemoteControl: ptr(cs.SupportsMediaControl),
		NowPlayingQueue: &[]dto.QueueItem{}, HasCustomDeviceName: ptr(false), ServerId: ptr(a.ServerID.String()),
		SupportedCommands: &cmds,
		Capabilities: &dto.ClientCapabilitiesDto{PlayableMediaTypes: &types, SupportedCommands: &cmds,
			SupportsMediaControl: ptr(cs.SupportsMediaControl), SupportsPersistentIdentifier: ptr(cs.SupportsPersistentIdentifier)},
	}
	if !cs.LastPlaybackCheckIn.IsZero() {
		d.LastPlaybackCheckIn = ptr(dto.NewTime(cs.LastPlaybackCheckIn))
	}
	if !cs.LastPaused.IsZero() {
		d.LastPausedDate = ptr(dto.NewTime(cs.LastPaused))
	}
	if cs.AppStoreURL != "" {
		d.Capabilities.AppStoreUrl = ptr(cs.AppStoreURL)
	}
	if cs.IconURL != "" {
		d.Capabilities.IconUrl = ptr(cs.IconURL)
	}
	if np := cs.NowPlaying; np != nil {
		if item, ok := items[np.ItemID]; ok {
			d.NowPlayingItem = &item
		}
		state.PositionTicks, state.CanSeek, state.IsPaused, state.IsMuted = ptr(np.PositionTicks), ptr(np.CanSeek), ptr(np.IsPaused), ptr(np.IsMuted)
		state.VolumeLevel, state.AudioStreamIndex, state.SubtitleStreamIndex = np.VolumeLevel, np.AudioStreamIndex, np.SubtitleStreamIndex
		if np.MediaSourceID != "" {
			state.MediaSourceId = ptr(np.MediaSourceID)
		}
		state.PlayMethod = ptr(dto.PlayMethod(np.PlayMethod))
		state.RepeatMode, state.PlaybackOrder = ptr(dto.RepeatMode(np.RepeatMode)), ptr(dto.PlaybackOrder(np.PlaybackOrder))
	}
	return d
}
