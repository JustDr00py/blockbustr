package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/cache"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
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
			IsInterlaced: s.IsInterlaced, IsDefault: s.IsDefault,
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
	rows := b.sources[it.ID]
	dtos := mediaSourceDtos(it, rows, b.streams, baseURL(r.Context()))

	session := PlaySession{
		UserID: user, DeviceID: s.DeviceID, ItemID: it.ID, Sources: map[string]media.Decision{},
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
	}
	if req.MaxStreamingBitrate != nil {
		opts.MaxBitrate = int64(*req.MaxStreamingBitrate)
	}
	u := urlParams{playSession: playSessionID, device: s.DeviceID, token: jfapi.AuthFrom(r.Context()).Token,
		audio: session.AudioStreamIndex, subtitle: session.SubtitleStreamIndex}

	out := make([]dto.MediaSourceInfo, 0, len(dtos))
	for i := range dtos {
		msID := *dtos[i].Id
		if req.MediaSourceId != nil && *req.MediaSourceId != "" && !sameID(*req.MediaSourceId, msID) {
			continue
		}
		// An unprobed source (a .strm before first play) can't be decided
		// yet: it keeps the details' answer until P2.4b probes it here.
		if i < len(rows) && len(b.streams[rows[i].ID]) > 0 {
			d := media.Decide(profile, decisionSource(rows[i], b.streams[rows[i].ID]), opts)
			applyDecision(&dtos[i], d, it, msID, u)
			session.Sources[msID] = d
		}
		out = append(out, dtos[i])
	}
	if a.Cache != nil {
		if err := a.Cache.SetJSON(r.Context(), cache.PlaySessionKey(playSessionID), session, cache.PlaySessionTTL); err != nil {
			a.internalError(w, r, err)
			return
		}
	}
	jfapi.WriteJSON(w, r, http.StatusOK, dto.PlaybackInfoResponse{MediaSources: &out, PlaySessionId: &playSessionID})
}

// playbackSources loads the item's sources, first probing a .strm that was
// never probed (TASKS P2.4b): its decision needs the streams and bitrate.
func (a *api) playbackSources(r *http.Request, it db.Item) (*itemBatch, error) {
	load := func() (*itemBatch, error) {
		b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
		return b, a.loadSources(r.Context(), b, []uuid.UUID{it.ID})
	}
	b, err := load()
	if err != nil || a.Probe == nil {
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
	if ms.MediaStreams == nil {
		return
	}
	for i := range *ms.MediaStreams {
		st := &(*ms.MediaStreams)[i]
		if st.Type == nil || *st.Type != dto.MediaStreamTypeSubtitle || st.Index == nil {
			continue
		}
		method, ok := d.Subtitles[int(*st.Index)]
		if !ok {
			continue
		}
		st.DeliveryMethod = ptr(dto.SubtitleDeliveryMethod(method))
		if method == "External" {
			st.DeliveryUrl = ptr("/Videos/" + dashed + "/" + msID + "/Subtitles/" + strconv.Itoa(int(*st.Index)) +
				"/0/Stream." + subtitleExt(deref(st.Codec)) + "?ApiKey=" + url.QueryEscape(u.token))
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
	add("SubtitleMethod", "Encode")
	if t.CopyVideo {
		add("AllowVideoStreamCopy", "true")
	}
	add("TranscodeReasons", strings.Join(d.Reasons, ","))
	return "/videos/" + dashedItem + "/master.m3u8?" + strings.Join(p, "&")
}
