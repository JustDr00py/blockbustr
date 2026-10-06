package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/media"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

// HLS remux/transcode (TASKS P2.6, DESIGN §8.3) behind PlaybackInfo's
// TranscodingUrl: master.m3u8 (one variant), main.m3u8 (the whole VOD
// playlist, written up front from the runtime so a seek works at once) and
// hls1/main/{n}.ts. Like Jellyfin, the URL carries everything needed
// (codecs, bitrates, track, copy flags), so nothing depends on Redis state:
// a restart or a flushed cache doesn't break a playback in progress.
//
// ffmpeg starts on the first segment request. A segment already written is
// served at once; one a little ahead of ffmpeg is waited for; anything else
// (a seek) restarts ffmpeg at that segment.

// Transcoding is the HLS setup handed to the handlers.
type Transcoding struct {
	Sessions       *transcode.Manager
	Encoder        transcode.Capability
	Device         string
	SegmentSeconds int

	locks sync.Map // play session id → *sync.Mutex: one start/restart at a time
}

func (t *Transcoding) lock(id string) *sync.Mutex {
	m, _ := t.locks.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (t *Transcoding) segmentSeconds() int {
	if t.SegmentSeconds > 0 {
		return t.SegmentSeconds
	}
	return transcode.DefaultSegmentSeconds
}

const (
	hlsStartTimeout = 30 * time.Second // ffmpeg's first segment
	hlsWaitTimeout  = 30 * time.Second // a segment ffmpeg is about to write
	hlsLookahead    = 4                // segments ahead of ffmpeg worth waiting for
)

func (a *api) registerHLS(rt *jfapi.Router) {
	rt.Get("/Videos/{itemId}/master.m3u8", a.requireUser(a.hlsMaster))
	rt.Head("/Videos/{itemId}/master.m3u8", a.requireUser(a.hlsMaster))
	rt.Get("/Videos/{itemId}/main.m3u8", a.requireUser(a.hlsMain))
	rt.Get("/Videos/{itemId}/hls1/{playlistId}/{segment}.{ext}", a.requireUser(a.hlsSegment))
	rt.Delete("/Videos/ActiveEncodings", a.requireUser(a.stopEncodings))
}

// hlsJob is what an HLS request is about.
type hlsJob struct {
	item        db.Item
	src         db.MediaSource
	streams     []db.MediaStream
	video       *db.MediaStream
	audio       *db.MediaStream
	q           jfapi.Query
	playSession string
}

// runtimeTicks is the source's duration (0 = unknown).
func (j hlsJob) runtimeTicks() int64 {
	if j.src.RuntimeTicks != nil {
		return *j.src.RuntimeTicks
	}
	return deref(j.item.RuntimeTicks)
}

// hlsRequest resolves the item, source and streams an HLS URL names. ok is
// false when a response was already written.
func (a *api) hlsRequest(w http.ResponseWriter, r *http.Request, s auth.Session) (hlsJob, bool) {
	var j hlsJob
	if a.Transcoding == nil {
		w.WriteHeader(http.StatusNotFound)
		return j, false
	}
	user, ok := queryUser(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return j, false
	}
	j.q = jfapi.QueryOf(r)
	j.playSession = j.q.Get("PlaySessionId")
	if j.playSession == "" {
		errorText(w, http.StatusBadRequest)
		return j, false
	}
	id, err := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return j, false
	}
	it, found, err := a.visibleItem(r, user, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return j, false
	}
	if !found || (it.Type != "Movie" && it.Type != "Episode") {
		w.WriteHeader(http.StatusNotFound)
		return j, false
	}
	b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
	if err := a.loadPlaySources(r.Context(), b, it); err != nil {
		a.internalError(w, r, err)
		return j, false
	}
	src, ok := streamSource(it, b.sources[it.ID], j.q.Get("MediaSourceId"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return j, false
	}
	j.item, j.src = it, src
	want := -1
	if n, ok := j.q.Int("AudioStreamIndex"); ok {
		want = n
	}
	streams := b.streams[src.ID]
	j.streams = streams
	for i := range streams {
		st := &streams[i]
		switch {
		case st.Type == "Video" && j.video == nil:
			j.video = st
		case st.Type == "Audio" && int(st.Idx) == want:
			j.audio = st
		}
	}
	if j.audio == nil { // the default track, else the first
		for i := range streams {
			if st := &streams[i]; st.Type == "Audio" && (j.audio == nil || (st.IsDefault && !j.audio.IsDefault)) {
				j.audio = st
			}
		}
	}
	return j, true
}

// tsCopyable are the codecs ffmpeg can copy into MPEG-TS segments; anything
// else (FLAC, PCM, Vorbis, ALAC, VP9…) is re-encoded even if the client's
// profile would take it.
var tsCopyable = map[string]bool{
	"h264": true, "hevc": true, "mpeg2video": true,
	"aac": true, "mp3": true, "mp2": true, "ac3": true, "eac3": true, "dts": true, "truehd": true, "opus": true,
}

// copiesAudio: the URL's AudioCodec list (or "copy") takes the source track
// as it is, within the channel limit, and TS can carry it.
func (j hlsJob) copiesAudio() bool {
	if j.audio == nil || !tsCopyable[deref(j.audio.Codec)] {
		return false
	}
	list := j.q.Get("AudioCodec")
	if strings.EqualFold(list, "copy") {
		return true
	}
	maxCh, _ := j.q.Int("TranscodingMaxAudioChannels")
	return list != "" && media.CodecIn(list, deref(j.audio.Codec)) && (maxCh <= 0 || int(deref(j.audio.Channels)) <= maxCh)
}

func (j hlsJob) copiesVideo() bool {
	allow, _ := j.q.Bool("AllowVideoStreamCopy")
	return allow && j.video != nil && tsCopyable[deref(j.video.Codec)] && media.CodecIn(j.q.Get("VideoCodec"), deref(j.video.Codec))
}

// firstOf is the first codec of a URL list that's in allowed (def if none).
func firstOf(list string, allowed []string, def string) string {
	for _, c := range strings.Split(list, ",") {
		if c = strings.ToLower(strings.TrimSpace(c)); slices.Contains(allowed, c) {
			return c
		}
	}
	return def
}

func isHDR(rangeType string) bool {
	return rangeType != "" && !strings.EqualFold(rangeType, "SDR")
}

// startOptions are the ffmpeg options the URL asks for.
func (a *api) startOptions(r *http.Request, j hlsJob, owner string) (transcode.StartOptions, error) {
	t := a.Transcoding
	o := transcode.StartOptions{
		Encoder: t.Encoder, Device: t.Device, SegmentSeconds: t.segmentSeconds(), AudioStream: -1, Owner: owner,
		CopyVideo: j.copiesVideo(), CopyAudio: j.copiesAudio(),
		VideoCodec: firstOf(j.q.Get("VideoCodec"), []string{"h264", "hevc"}, "h264"),
	}
	if n, err := strconv.ParseInt(j.q.Get("VideoBitrate"), 10, 64); err == nil {
		o.VideoBitrate = n
	}
	if j.audio != nil {
		o.AudioStream = int(j.audio.Idx)
		if !o.CopyAudio {
			o.AudioCodec = firstOf(j.q.Get("AudioCodec"), []string{"aac", "mp3", "ac3", "eac3", "opus", "flac"}, "aac")
			if n, err := strconv.ParseInt(j.q.Get("AudioBitrate"), 10, 64); err == nil {
				o.AudioBitrate = n
			}
			if maxCh, ok := j.q.Int("TranscodingMaxAudioChannels"); ok && maxCh > 0 && int(deref(j.audio.Channels)) > maxCh {
				o.AudioChannels = maxCh
			}
		}
	}
	if err := a.burnOptions(r, j, &o, j.streams); err != nil {
		return o, err
	}
	if !o.CopyVideo && j.video != nil {
		o.Tonemap = isHDR(deref(j.video.VideoRangeType))
	}
	o.Input = j.src.PathOrUrl
	if j.src.IsRemote || !strings.EqualFold(j.src.Protocol, "File") {
		if a.Resolver == nil {
			return o, errors.New("remote source without a resolver")
		}
		link, err := a.Resolver.Resolve(r.Context(), resolve.FromURL(j.src.PathOrUrl))
		if err != nil {
			return o, err
		}
		o.Input, o.Remote = link.URL, true
	}
	return o, nil
}

// avcProfiles are H.264 profile_idc/constraint bytes by ffprobe profile name.
var avcProfiles = map[string]string{
	"constrained baseline": "42E0", "baseline": "4200", "main": "4D00", "extended": "5800",
	"high": "6400", "high 10": "6E00", "high 4:2:2": "7A00", "high 4:4:4 predictive": "F400",
}

// avcCodec is the RFC 6381 name of a copied H.264 stream (ffprobe level 40 =
// 4.0 → 0x28); High@4.1 when the profile isn't known.
func avcCodec(profile string, level float32) string {
	p, ok := avcProfiles[strings.ToLower(profile)]
	if !ok || level <= 0 {
		return "avc1.640029"
	}
	return fmt.Sprintf("avc1.%s%02x", strings.ToLower(p), int(level))
}

// hlsCodecs is the variant's CODECS attribute (RFC 8216 §4.3.4.2).
func hlsCodecs(j hlsJob, o transcode.StartOptions) string {
	video := "avc1.640029" // H.264 High@4.1, what the hardware encoders produce
	switch {
	case o.CopyVideo && j.video != nil && deref(j.video.Codec) == "h264":
		video = avcCodec(deref(j.video.Profile), deref(j.video.Level))
	case o.CopyVideo && j.video != nil && deref(j.video.Codec) == "hevc":
		video = "hvc1.2.4.L150.B0"
	case !o.CopyVideo && o.VideoCodec == "hevc":
		video = "hvc1.1.6.L120.90"
	}
	acodec := o.AudioCodec
	if o.CopyAudio && j.audio != nil {
		acodec = deref(j.audio.Codec)
	}
	audio := map[string]string{"aac": "mp4a.40.2", "mp3": "mp4a.40.34", "ac3": "ac-3", "eac3": "ec-3", "opus": "Opus", "flac": "fLaC"}[acodec]
	if audio == "" || j.audio == nil {
		return video
	}
	return video + "," + audio
}

// withQuery returns raw with key set to value (or appended), keeping the
// other parameters as the client sent them.
func withQuery(raw, key, value string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if k, _, _ := strings.Cut(p, "="); strings.EqualFold(k, key) {
			parts[i] = key + "=" + value
			return strings.Join(parts, "&")
		}
	}
	return raw + "&" + key + "=" + value
}

func (a *api) hlsMaster(w http.ResponseWriter, r *http.Request, s auth.Session) {
	j, ok := a.hlsRequest(w, r, s)
	if !ok {
		return
	}
	o, err := a.startOptions(r, j, s.DeviceID)
	if err != nil {
		a.Log.WarnContext(r.Context(), "hls source unavailable", "item", j.item.ID, "err", err)
		sourceUnavailable(w, err, http.StatusInternalServerError)
		return
	}
	// Video plus audio, copied or not (6,000,000 = 5,839,539 + 160,461).
	audioBitrate, _ := strconv.ParseInt(j.q.Get("AudioBitrate"), 10, 64)
	bandwidth := o.VideoBitrate + audioBitrate
	if o.VideoBitrate <= 0 {
		bandwidth = int64(deref(j.src.Bitrate))
	}
	attrs := []string{fmt.Sprintf("BANDWIDTH=%d", bandwidth), fmt.Sprintf("AVERAGE-BANDWIDTH=%d", bandwidth)}
	vr := "SDR"
	if o.CopyVideo && j.video != nil && isHDR(deref(j.video.VideoRangeType)) {
		vr = "PQ"
		if strings.Contains(deref(j.video.VideoRangeType), "HLG") {
			vr = "HLG"
		}
	}
	attrs = append(attrs, "VIDEO-RANGE="+vr, `CODECS="`+hlsCodecs(j, o)+`"`)
	if j.video != nil && deref(j.video.Width) > 0 {
		attrs = append(attrs, fmt.Sprintf("RESOLUTION=%dx%d", deref(j.video.Width), deref(j.video.Height)))
	}
	if j.video != nil && deref(j.video.RealFrameRate) > 0 {
		attrs = append(attrs, "FRAME-RATE="+strconv.FormatFloat(float64(deref(j.video.RealFrameRate)), 'f', -1, 32))
	}
	query := r.URL.RawQuery
	if o.CopyAudio {
		query = withQuery(query, "AudioCodec", "copy") // as Jellyfin's main.m3u8 link does
	}
	body := "#EXTM3U\n#EXT-X-STREAM-INF:" + strings.Join(attrs, ",") + "\nmain.m3u8?" + query + "\n"
	writePlaylist(w, r, body)
}

func writePlaylist(w http.ResponseWriter, r *http.Request, body string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// segmentCount is how many segments of seg seconds cover runtime.
func segmentCount(runtime int64, seg int) int {
	segTicks := int64(seg) * 10_000_000
	return int((runtime + segTicks - 1) / segTicks)
}

func (a *api) hlsMain(w http.ResponseWriter, r *http.Request, s auth.Session) {
	j, ok := a.hlsRequest(w, r, s)
	if !ok {
		return
	}
	runtime := j.runtimeTicks()
	if runtime <= 0 {
		a.Log.WarnContext(r.Context(), "hls: unknown runtime", "item", j.item.ID)
		errorText(w, http.StatusInternalServerError)
		return
	}
	seg := a.Transcoding.segmentSeconds()
	segTicks := int64(seg) * 10_000_000
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:0\n", seg)
	for n := range segmentCount(runtime, seg) {
		start := int64(n) * segTicks
		length := min(segTicks, runtime-start)
		fmt.Fprintf(&b, "#EXTINF:%.6f, nodesc\nhls1/main/%d.ts?%s&runtimeTicks=%d&actualSegmentLengthTicks=%d\n",
			float64(length)/1e7, n, r.URL.RawQuery, start, length)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	writePlaylist(w, r, b.String())
}

func (a *api) hlsSegment(w http.ResponseWriter, r *http.Request, s auth.Session) {
	n, err := strconv.Atoi(jfapi.URLParam(r, "segment"))
	if err != nil || n < 0 || !strings.EqualFold(jfapi.URLParam(r, "ext"), "ts") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	j, ok := a.hlsRequest(w, r, s)
	if !ok {
		return
	}
	if rt := j.runtimeTicks(); rt > 0 && n >= segmentCount(rt, a.Transcoding.segmentSeconds()) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sess, err := a.sessionFor(r, j, n, s.DeviceID)
	if err == nil && !sess.HasSegment(n) {
		err = sess.WaitSegment(r.Context(), n, hlsWaitTimeout)
		if err != nil { // restarted under us by another request? wait on the new run
			if again, ok := a.Transcoding.Sessions.Get(j.playSession); ok && again != sess {
				err = again.WaitSegment(r.Context(), n, hlsWaitTimeout)
				sess = again
			}
		}
	}
	if err != nil {
		if r.Context().Err() == nil {
			a.Log.WarnContext(r.Context(), "hls segment failed", "item", j.item.ID, "segment", n, "err", err)
			errorText(w, http.StatusInternalServerError)
		}
		return
	}
	f, err := os.Open(sess.SegmentPath(n))
	if err != nil {
		errorText(w, http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		errorText(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// sessionFor returns the ffmpeg session that has or will soon write segment
// n: the running one when n is written or just ahead of it, else a new run
// starting at n (first request, or a seek).
func (a *api) sessionFor(r *http.Request, j hlsJob, n int, owner string) (*transcode.Session, error) {
	t := a.Transcoding
	l := t.lock(j.playSession)
	l.Lock()
	defer l.Unlock()
	if sess, ok := t.Sessions.Get(j.playSession); ok {
		if servesSoon(sess, n) {
			return sess, nil
		}
		sess, _, err := t.Sessions.Restart(r.Context(), j.playSession, n, hlsStartTimeout)
		return sess, err
	}
	o, err := a.startOptions(r, j, owner)
	if err != nil {
		return nil, err
	}
	o.StartSegment = n
	return t.Sessions.Start(r.Context(), j.playSession, o, hlsStartTimeout)
}

// servesSoon: the session has segment n, or ffmpeg will reach it within
// hlsLookahead segments; otherwise a seek restarts it at n.
func servesSoon(s *transcode.Session, n int) bool {
	return s.HasSegment(n) || (n >= s.Opts.StartSegment && n <= s.Next()+hlsLookahead)
}

// stopEncodings ends a play session's transcode, or every one of a device
// (DELETE /Videos/ActiveEncodings), as clients do when playback stops.
func (a *api) stopEncodings(w http.ResponseWriter, r *http.Request, s auth.Session) {
	if a.Transcoding != nil {
		q := jfapi.QueryOf(r)
		switch {
		case q.Get("playSessionId") != "":
			a.Transcoding.Sessions.Close(q.Get("playSessionId"))
		case q.Get("deviceId") != "":
			a.Transcoding.Sessions.CloseOwner(q.Get("deviceId"))
		default:
			a.Transcoding.Sessions.CloseOwner(s.DeviceID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
