package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/resolve"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
	"github.com/sysadmin/blockbustr/internal/subtitles"
	"github.com/sysadmin/blockbustr/internal/transcode"
)

// Subtitle files (TASKS P2.8): the External DeliveryUrls PlaybackInfo hands
// out, /Videos/{id}/{msId}/Subtitles/{idx}[/{startTicks}]/Stream.{fmt}.
// Public like /Videos/{id}/stream (players fetch them without headers; the
// URL also carries ApiKey). Embedded text tracks are extracted once and
// cached (subtitles.Store); sidecar files are served as they are.

func (a *api) registerSubtitles(rt *jfapi.Router) {
	for _, p := range []string{
		"/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/Stream.{format}",
		"/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/{startTicks}/Stream.{format}",
	} {
		rt.Get(p, a.subtitleFile)
	}
}

// subtitleSource describes a media source's subtitle tracks for the store.
// The key changes with the source's etag, so a replaced file is re-read.
func (a *api) subtitleSource(r *http.Request, src db.MediaSource, streams []db.MediaStream) (subtitles.Source, error) {
	sum := sha256.Sum256([]byte(src.ID.String() + "|" + deref(src.Etag)))
	out := subtitles.Source{Key: hex.EncodeToString(sum[:12]), Input: src.PathOrUrl}
	for _, st := range streams {
		if st.Type == "Subtitle" {
			out.Tracks = append(out.Tracks, subtitles.Track{Index: int(st.Idx), Codec: deref(st.Codec), Path: deref(st.ExternalPath)})
		}
	}
	if src.IsRemote || !strings.EqualFold(src.Protocol, "File") {
		if a.Resolver == nil {
			return out, errors.New("remote source without a resolver")
		}
		link, err := a.Resolver.Resolve(r.Context(), resolve.FromURL(src.PathOrUrl))
		if err != nil {
			return out, err
		}
		out.Input, out.Remote = link.URL, true
	}
	return out, nil
}

func (a *api) subtitleFile(w http.ResponseWriter, r *http.Request) {
	format := subtitles.Formats[strings.ToLower(jfapi.URLParam(r, "format"))]
	index, err := strconv.Atoi(jfapi.URLParam(r, "index"))
	id, idErr := dto.ParseID(jfapi.URLParam(r, "itemId"))
	if a.Subtitles == nil || format == "" || err != nil || idErr != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var start time.Duration
	if t := jfapi.URLParam(r, "startTicks"); t != "" {
		ticks, err := strconv.ParseInt(t, 10, 64)
		if err != nil || ticks < 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		start = time.Duration(ticks * 100)
	}
	it, found, err := a.visibleItem(r, uuid.Nil, id.UUID())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !found || (it.Type != "Movie" && it.Type != "Episode") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b := &itemBatch{sources: map[uuid.UUID][]db.MediaSource{}, streams: map[uuid.UUID][]db.MediaStream{}}
	if err := a.loadPlaySources(r.Context(), b, it); err != nil {
		a.internalError(w, r, err)
		return
	}
	src, ok := streamSource(it, b.sources[it.ID], jfapi.URLParam(r, "mediaSourceId"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ss, err := a.subtitleSource(r, src, b.streams[src.ID])
	if err != nil {
		a.Log.WarnContext(r.Context(), "subtitle source unavailable", "item", it.ID, "err", err)
		errorText(w, http.StatusInternalServerError)
		return
	}
	file, err := a.Subtitles.File(r.Context(), ss, index)
	if err != nil {
		if !errors.Is(err, subtitles.ErrNotText) && !strings.Contains(err.Error(), "no track") {
			a.Log.WarnContext(r.Context(), "subtitle extraction failed", "item", it.ID, "index", index, "err", err)
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := subtitles.Convert(r.Context(), file, format, start)
	if err != nil {
		a.Log.WarnContext(r.Context(), "subtitle conversion failed", "item", it.ID, "index", index, "err", err)
		errorText(w, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", subtitles.ContentType(format))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// burnOptions adds the subtitle the URL asks to burn in (SubtitleStreamIndex
// with SubtitleMethod=Encode) to o: image tracks are overlaid from the input,
// text tracks rendered from their extracted file. Burning needs an encode,
// so the video is no longer copied.
func (a *api) burnOptions(r *http.Request, j hlsJob, o *transcode.StartOptions, streams []db.MediaStream) error {
	idx, ok := j.q.Int("SubtitleStreamIndex")
	if !ok || idx < 0 || !strings.EqualFold(j.q.Get("SubtitleMethod"), "Encode") {
		return nil
	}
	var st *db.MediaStream
	for i := range streams {
		if streams[i].Type == "Subtitle" && int(streams[i].Idx) == idx {
			st = &streams[i]
		}
	}
	if st == nil {
		return nil // nothing to burn: play without
	}
	o.CopyVideo = false
	if !subtitles.IsText(deref(st.Codec)) {
		if st.IsExternal {
			return nil // external image subtitles (.sub/.idx) aren't supported
		}
		o.BurnImage = ptr(idx)
		return nil
	}
	if a.Subtitles == nil {
		return errors.New("text subtitle burn-in without a subtitle store")
	}
	ss, err := a.subtitleSource(r, j.src, streams)
	if err != nil {
		return err
	}
	file, err := a.Subtitles.File(r.Context(), ss, idx)
	if err != nil {
		return err
	}
	if st.IsExternal { // a user's file name; give libass a cache copy with a safe name
		file, err = a.Subtitles.Copy(file)
		if err != nil {
			return err
		}
	}
	o.BurnText = file
	return nil
}
