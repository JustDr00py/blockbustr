package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/stremio"
)

// PlaybackInfo (TASKS P2.2, DESIGN §8.1): the item's media sources with the
// play decision for the requesting device (media.Decide), a new
// PlaySessionId, and the decisions stored in Redis for the stream and HLS
// endpoints (P2.3, P2.6).

func (a *api) registerPlayback(rt *jfapi.Router) {
	rt.Get("/Items/{itemId}/PlaybackInfo", a.requireUser(a.playbackInfo))
	rt.Post("/Items/{itemId}/PlaybackInfo", a.requireUser(a.playbackInfo))
}

// PlaySession is what a PlaybackInfo call decided, keyed by its
// PlaySessionId (cache.PlaySessionKey).
type PlaySession struct {
	UserID              uuid.UUID
	DeviceID            string
	ItemID              uuid.UUID
	Sources             map[string]media.Decision // by media source id (N form)
	AudioStreamIndex    *int
	SubtitleStreamIndex *int
	StartTimeTicks      int64
	// Order is an addon title's sources as offered, first (the item id,
	// the default) first; stream and HLS requests of the session use it.
	Order []uuid.UUID `json:",omitempty"`
	// MaxHeight and MaxSize (bytes) are the user's caps on addon versions
	// (0: none), for stream requests, which carry no token.
	MaxHeight int   `json:",omitempty"`
	MaxSize   int64 `json:",omitempty"`
}

// playbackRequest merges the PlaybackInfoDto body with the query string
// (body values win; GET has only the query).
func playbackRequest(w http.ResponseWriter, r *http.Request) (dto.PlaybackInfoDto, error) {
	var b dto.PlaybackInfoDto
	if r.Method == http.MethodPost {
		if err := jfapi.DecodeJSON(w, r, &b); err != nil {
			return b, err
		}
	}
	q := jfapi.QueryOf(r)
	intQ := func(name string) *int32 {
		if n, ok := q.Int(name); ok {
			return ptr(int32(n))
		}
		return nil
	}
	boolQ := func(name string) *bool {
		if v, ok := q.Bool(name); ok {
			return &v
		}
		return nil
	}
	if b.MaxStreamingBitrate == nil {
		b.MaxStreamingBitrate = intQ("maxStreamingBitrate")
	}
	if b.AudioStreamIndex == nil {
		b.AudioStreamIndex = intQ("audioStreamIndex")
	}
	if b.SubtitleStreamIndex == nil {
		b.SubtitleStreamIndex = intQ("subtitleStreamIndex")
	}
	if b.MediaSourceId == nil && q.Get("mediaSourceId") != "" {
		b.MediaSourceId = ptr(q.Get("mediaSourceId"))
	}
	if b.StartTimeTicks == nil {
		if v, err := strconv.ParseInt(q.Get("startTimeTicks"), 10, 64); err == nil {
			b.StartTimeTicks = &v
		}
	}
	for _, f := range []struct {
		dst  **bool
		name string
	}{{&b.EnableDirectPlay, "enableDirectPlay"}, {&b.EnableDirectStream, "enableDirectStream"}, {&b.EnableTranscoding, "enableTranscoding"}} {
		if *f.dst == nil {
			*f.dst = boolQ(f.name)
		}
	}
	return b, nil
}

// deviceProfile converts the request's profile: the media types mirror
// Jellyfin's JSON, so a round trip maps every field.
func deviceProfile(p *dto.DeviceProfile) (media.DeviceProfile, error) {
	var out media.DeviceProfile
	if p == nil {
		return out, nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

// decisionSource is a stored source as the decision engine sees it.
func decisionSource(src db.MediaSource, streams []db.MediaStream) media.Source {
	out := media.Source{Container: deref(src.Container), Bitrate: int64(deref(src.Bitrate)), Remote: src.IsRemote}
	for _, s := range streams {
		out.Streams = append(out.Streams, media.Stream{
			Index: int(s.Idx), Type: media.StreamType(s.Type), Codec: deref(s.Codec), Profile: deref(s.Profile),
			Level: int(deref(s.Level)), BitDepth: int(deref(s.BitDepth)), Width: int(deref(s.Width)), Height: int(deref(s.Height)),
			Bitrate: int64(deref(s.Bitrate)), Channels: int(deref(s.Channels)), SampleRate: int(deref(s.SampleRate)),
			VideoRangeType: deref(s.VideoRangeType), RealFrameRate: float64(deref(s.RealFrameRate)),
			IsInterlaced: s.IsInterlaced, IsDefault: s.IsDefault, IsExternal: s.IsExternal,
		})
	}
	return out
}

func optInt(p *int32) *int {
	if p == nil {
		return nil
	}
	n := int(*p)
	return &n
}

func (a *api) playbackInfo(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	req, err := playbackRequest(w, r)
	if err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	if req.UserId != nil && req.UserId.UUID() != user && !s.IsAdmin {
		errorText(w, http.StatusForbidden)
		return
	}
	if s.NoPlayback { // the policy's EnableMediaPlayback is off
		errorText(w, http.StatusForbidden)
		return
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	it, found, err := a.visibleItem(r, user, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !found || (it.Type != "Movie" && it.Type != "Episode") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	profile, err := deviceProfile(req.DeviceProfile)
	if err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}

	b, err := a.playbackSources(r, it)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var maxBitrate int64
	if req.MaxStreamingBitrate != nil {
		maxBitrate = int64(*req.MaxStreamingBitrate)
	}
	session := PlaySession{
		UserID: user, DeviceID: s.DeviceID, ItemID: it.ID, Sources: map[string]media.Decision{}, MaxHeight: s.MaxHeight, MaxSize: s.MaxSize,
		AudioStreamIndex: optInt(req.AudioStreamIndex), SubtitleStreamIndex: optInt(req.SubtitleStreamIndex),
	}
	if req.StartTimeTicks != nil {
		session.StartTimeTicks = *req.StartTimeTicks
	}
	playSessionID := dto.IDFromUUID(uuid.New()).String()
	opts := media.PlayOptions{
		AudioStreamIndex: session.AudioStreamIndex, SubtitleStreamIndex: session.SubtitleStreamIndex,
		DisableDirectPlay:   req.EnableDirectPlay != nil && !*req.EnableDirectPlay,
		DisableDirectStream: req.EnableDirectStream != nil && !*req.EnableDirectStream,
		DisableTranscoding:  req.EnableTranscoding != nil && !*req.EnableTranscoding,
		// The user's policy or the server's encoders may rule out
		// re-encoding video: a version that plays as is (or remuxed) is
		// offered first, and the rest only with their video copied.
		NoVideoEncode: s.NoTranscode || !a.Transcoding.encodes(),
		NoCPUFilters:  a.Transcoding != nil && a.Transcoding.NoCPU.Load(),
	}
	opts.MaxBitrate = maxBitrate

	rows := b.sources[it.ID]
	allSources := false
	if _, _, ok := stremioRef(it); ok && len(rows) == 0 {
		prefs := streamPrefs(profile, maxBitrate)
		prefs.MaxHeight = minCap(prefs.MaxHeight, s.MaxHeight)
		prefs.MaxSize = s.MaxSize
		rows = a.offerStreams(r, b, it, prefs, req.MediaSourceId)
		// The item id stands for the best choice: a client asking for it
		// (from the details' placeholder) gets every choice.
		allSources = req.MediaSourceId != nil && sameID(*req.MediaSourceId, dto.IDFromUUID(it.ID).String())
		// Sorted before ids are given out, so the item id (what clients
		// send for "the default") names the first version offered: one
		// that plays as is when there is one. Stream and HLS requests of
		// this session read the same order (inPlayOrder).
		rows = playableFirst(rows, b.streams, profile, opts)
		for _, row := range rows {
			session.Order = append(session.Order, row.ID)
		}
	}
	dtos := mediaSourceDtos(it, rows, b.streams, baseURL(r.Context()), a.StreamSigner)
	u := urlParams{playSession: playSessionID, device: s.DeviceID, token: jfapi.AuthFrom(r.Context()).Token,
		audio: session.AudioStreamIndex, subtitle: session.SubtitleStreamIndex}

	out := make([]dto.MediaSourceInfo, 0, len(dtos))
	for i := range dtos {
		msID := *dtos[i].Id
		if !allSources && req.MediaSourceId != nil && *req.MediaSourceId != "" && !sameID(*req.MediaSourceId, msID) &&
			(i >= len(rows) || !sameID(*req.MediaSourceId, dto.IDFromUUID(rows[i].ID).String())) {
			continue
		}
		// An unprobed source (a .strm before first play, an addon stream)
		// can't be decided: it keeps the details' answer, plus delivery of
		// any external subtitles (an addon's) as files.
		switch {
		case i < len(rows) && hasVideo(b.streams[rows[i].ID]):
			d := media.Decide(profile, decisionSource(rows[i], b.streams[rows[i].ID]), opts)
			applyDecision(&dtos[i], d, it, msID, u)
			session.Sources[msID] = d
		case i < len(rows):
			deliverSubtitles(&dtos[i], externalTextSubtitles(b.streams[rows[i].ID]), it.ID.String(), msID, u.token)
		}
		out = append(out, dtos[i])
	}
	// A version that plays as is beats one that needs the transcoder: the
	// client takes the first source, and switching versions is free where
	// transcoding spends the CPU. Rank works from addon labels, so an
	// over-cap source with no size hint can still rank first; its probe
	// knows better, and this order is where that correction lands. Rank
	// order is kept within each group, and undecided sources (unprobed,
	// uncached choices) stay behind the decided ones.
	playsAsIs := func(ms dto.MediaSourceInfo) bool {
		d, ok := session.Sources[*ms.Id]
		return ok && (d.Mode == media.ModeDirect || d.Mode == media.ModeRemux)
	}
	slices.SortStableFunc(out, func(a, b dto.MediaSourceInfo) int {
		switch {
		case playsAsIs(a) && !playsAsIs(b):
			return -1
		case playsAsIs(b) && !playsAsIs(a):
			return 1
		}
		return 0
	})
	if a.Cache != nil {
		if err := a.Cache.SetJSON(r.Context(), cache.PlaySessionKey(playSessionID), session, cache.PlaySessionTTL); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.PlaybackInfoResponse{MediaSources: &out, PlaySessionId: &playSessionID})
}

// offerStreams picks a catalog item's streams for this client (DESIGN
// §7.3) and returns them as sources, best first. An earlier choice the
// client asks for by id is added after them. When collection fails or finds
// nothing, the item keeps its placeholder.
func (a *api) offerStreams(r *http.Request, b *itemBatch, it db.Item, prefs stremio.Prefs, want *string) []db.MediaSource {
	pick, err := a.pickStreams(r.Context(), it, prefs)
	if err != nil {
		a.Log.WarnContext(r.Context(), "stream collection failed", "item", it.ID, "err", err)
	}
	choices := pick.Choices
	if want != nil && *want != "" && !sameID(*want, dto.IDFromUUID(it.ID).String()) {
		if set, ok := a.loadStreamSet(r.Context(), it.ID); ok {
			for _, c := range withinCaps(set.All(), capsOf(r.Context())) {
				if sameID(*want, dto.IDFromUUID(c.ID).String()) && !hasChoice(choices, c.ID) {
					choices = append(choices, c)
				}
			}
		}
	}
	// Probe the ready choices (waiting a little): their tracks let the
	// client switch audio, and start in the preferred language. The one
	// the client asked for goes first, so a probe limit never skips it.
	a.probeChoices(r.Context(), pickedFirst(choices, want), probeWait)
	rows, streams := choiceSources(it, choices, pick.Subtitles)
	applyProbes(rows, streams, a.loadChoiceProbes(r.Context(), choices), a.preferredAudio())
	for id, st := range streams {
		b.streams[id] = st
	}
	return rows
}

// pickedFirst is choices with the one want names (a choice id) moved to
// the front; as they are when want names none.
func pickedFirst(choices []StreamChoice, want *string) []StreamChoice {
	if want == nil || *want == "" {
		return choices
	}
	i := slices.IndexFunc(choices, func(c StreamChoice) bool { return sameID(*want, dto.IDFromUUID(c.ID).String()) })
	if i <= 0 {
		return choices
	}
	out := make([]StreamChoice, 0, len(choices))
	out = append(out, choices[i])
	out = append(out, choices[:i]...)
	return append(out, choices[i+1:]...)
}

// playbackSources loads the item's sources, first probing a .strm that was
// never probed (TASKS P2.4b): its decision needs the streams and bitrate.
func (a *api) playbackSources(r *http.Request, it db.Item) (*itemBatch, error) {
	load := func() (*itemBatch, error) {
		b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
		return b, a.loadSources(r.Context(), b, []uuid.UUID{it.ID})
	}
	b, err := load()
	if _, _, catalog := stremioRef(it); err != nil || a.Probe == nil || catalog {
		return b, err
	}
	for _, src := range b.sources[it.ID] {
		if src.IsRemote && src.ProbedAt == nil && len(b.streams[src.ID]) == 0 {
			if _, err := a.Probe.ProbeRemote(r.Context(), it.ID); err != nil {
				return nil, err
			}
			return load()
		}
	}
	return b, nil
}

// sameID compares ids in any spelling (N, dashed, upper case).
func sameID(a, b string) bool {
	x, err1 := dto.ParseID(a)
	y, err2 := dto.ParseID(b)
	return err1 == nil && err2 == nil && x == y
}

// urlParams are the per-request values the transcoding and subtitle URLs
// carry.
type urlParams struct {
	playSession, device, token string
	audio, subtitle            *int
}

// applyDecision sets a source's flags, container, transcoding URL and
// subtitle delivery from the decision, as 12.1.0 reports them.
func applyDecision(ms *dto.MediaSourceInfo, d media.Decision, it db.Item, msID string, u urlParams) {
	ms.SupportsDirectPlay, ms.SupportsDirectStream, ms.SupportsTranscoding = ptr(d.SupportsDirectPlay), ptr(d.SupportsDirectStream), ptr(d.SupportsTranscoding)
	if d.Container != "" {
		ms.Container = ptr(d.Container)
	}
	dashed := it.ID.String()
	if t := d.Transcode; t != nil && !d.SupportsDirectPlay {
		audio := u.audio
		if audio == nil && ms.DefaultAudioStreamIndex != nil {
			audio = ptr(int(*ms.DefaultAudioStreamIndex))
		}
		ms.TranscodingUrl = ptr(transcodingURL(dashed, msID, deref(ms.ETag), d, u, audio))
		ms.TranscodingSubProtocol = ptr(dto.MediaStreamProtocol(t.Protocol))
		ms.TranscodingContainer = ptr(t.Container)
	}
	deliverSubtitles(ms, d.Subtitles, dashed, msID, u.token)
}

func hasVideo(streams []db.MediaStream) bool {
	for _, s := range streams {
		if s.Type == "Video" {
			return true
		}
	}
	return false
}

// externalTextSubtitles are the delivery methods for an undecided source:
// its external text subtitles as files.
func externalTextSubtitles(streams []db.MediaStream) map[int]string {
	out := map[int]string{}
	for _, s := range streams {
		if s.Type == "Subtitle" && s.IsExternal && textSubtitleCodecs[deref(s.Codec)] {
			out[int(s.Idx)] = "External"
		}
	}
	return out
}

// deliverSubtitles sets each subtitle's DeliveryMethod from methods (by
// index) and, for External, its DeliveryUrl.
func deliverSubtitles(ms *dto.MediaSourceInfo, methods map[int]string, dashed, msID, token string) {
	if ms.MediaStreams == nil {
		return
	}
	for i := range *ms.MediaStreams {
		st := &(*ms.MediaStreams)[i]
		if st.Type == nil || *st.Type != dto.MediaStreamTypeSubtitle || st.Index == nil {
			continue
		}
		method, ok := methods[int(*st.Index)]
		if !ok {
			continue
		}
		st.DeliveryMethod = ptr(dto.SubtitleDeliveryMethod(method))
		if method == "External" {
			st.DeliveryUrl = ptr("/Videos/" + dashed + "/" + msID + "/Subtitles/" + strconv.Itoa(int(*st.Index)) +
				"/0/Stream." + subtitleExt(deref(st.Codec)) + "?ApiKey=" + url.QueryEscape(token))
		}
	}
}

// subtitleExt is the format a text subtitle is served in: SRT for SubRip,
// ASS/SSA as is (observed), WebVTT otherwise.
func subtitleExt(codec string) string {
	switch strings.ToLower(codec) {
	case "subrip", "srt":
		return "srt"
	case "ass", "ssa":
		return strings.ToLower(codec)
	}
	return "vtt"
}

// transcodingURL is the HLS master playlist URL in Jellyfin's parameter
// names and order. Our HLS endpoint (P2.6) reads it back; clients treat it
// as opaque. Jellyfin's per-codec hints (av1-level=…) are left out.
func transcodingURL(dashedItem, msID, etag string, d media.Decision, u urlParams, audio *int) string {
	t := d.Transcode
	var p []string
	add := func(k, v string) {
		p = append(p, k+"="+strings.ReplaceAll(url.QueryEscape(v), "%2C", ","))
	}
	add("DeviceId", u.device)
	add("MediaSourceId", msID)
	add("VideoCodec", strings.Join(t.VideoCodecs, ","))
	add("AudioCodec", strings.Join(t.AudioCodecs, ","))
	if audio != nil {
		add("AudioStreamIndex", strconv.Itoa(*audio))
	}
	if u.subtitle != nil && *u.subtitle >= 0 {
		add("SubtitleStreamIndex", strconv.Itoa(*u.subtitle))
	}
	if t.VideoBitrate > 0 {
		add("VideoBitrate", strconv.FormatInt(t.VideoBitrate, 10))
	}
	if t.AudioBitrate > 0 {
		add("AudioBitrate", strconv.FormatInt(t.AudioBitrate, 10))
	}
	if t.MaxFramerate > 0 {
		add("MaxFramerate", strconv.FormatFloat(t.MaxFramerate, 'f', -1, 32)) // stored as float32
	}
	add("SegmentContainer", t.Container)
	add("PlaySessionId", u.playSession)
	add("ApiKey", u.token)
	if t.MaxAudioChannels > 0 {
		add("TranscodingMaxAudioChannels", strconv.Itoa(t.MaxAudioChannels))
	}
	add("RequireAvc", "false")
	add("EnableAudioVbrEncoding", "true")
	add("Tag", etag)
	// The chosen subtitle's delivery (Encode = burn in, read by the HLS
	// endpoint); Jellyfin sends Encode when none is chosen.
	method := "Encode"
	if u.subtitle != nil && *u.subtitle >= 0 && d.Subtitles[*u.subtitle] != "" {
		method = d.Subtitles[*u.subtitle]
	}
	add("SubtitleMethod", method)
	if t.CopyVideo {
		add("AllowVideoStreamCopy", "true")
	}
	add("TranscodeReasons", strings.Join(d.Reasons, ","))
	return "/videos/" + dashedItem + "/master.m3u8?" + strings.Join(p, "&")
}

// playableFirst puts the sources that play as is for this client (direct
// play or remux, judged from their probe) ahead of the rest, keeping rank
// order within each group. Unprobed sources can't be judged and stay
// behind.
func playableFirst(rows []db.MediaSource, streams map[uuid.UUID][]db.MediaStream, p media.DeviceProfile, opts media.PlayOptions) []db.MediaSource {
	asIs := map[uuid.UUID]bool{}
	for _, row := range rows {
		if hasVideo(streams[row.ID]) {
			d := media.Decide(p, decisionSource(row, streams[row.ID]), opts)
			asIs[row.ID] = d.Mode == media.ModeDirect || d.Mode == media.ModeRemux
		}
	}
	out := slices.Clone(rows)
	slices.SortStableFunc(out, func(a, b db.MediaSource) int {
		switch {
		case asIs[a.ID] && !asIs[b.ID]:
			return -1
		case asIs[b.ID] && !asIs[a.ID]:
			return 1
		}
		return 0
	})
	return out
}
