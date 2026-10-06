package handlers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The recon library (TASKS P1.11): every item, person, genre and studio that
// appears in any captured Jellyfin response, seeded under the SAME ids. Then
// captured requests that name items (ids=, parentId=, personIds=…) replay
// unchanged and return real items to compare shapes against.

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// captureUserID is the scrubbed id of the user every capture was made as.
const captureUserID = "bbbb0000-0000-0000-0000-000000000001"

type jmap = map[string]any

func str(m jmap, k string) string { s, _ := m[k].(string); return s }

func strp(m jmap, k string) *string {
	if s, ok := m[k].(string); ok && s != "" {
		return &s
	}
	return nil
}

func nump(m jmap, k string) *float64 {
	if f, ok := m[k].(float64); ok {
		return &f
	}
	return nil
}

func intp(m jmap, k string) *int64 {
	if f, ok := m[k].(float64); ok {
		n := int64(f)
		return &n
	}
	return nil
}

func objs(m jmap, k string) []jmap {
	var out []jmap
	if list, ok := m[k].([]any); ok {
		for _, v := range list {
			if o, ok := v.(jmap); ok {
				out = append(out, o)
			}
		}
	}
	return out
}

func timep(m jmap, k string) *time.Time {
	if s := str(m, k); s != "" {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil && t.Year() > 1 {
			return &t
		}
	}
	return nil
}

var reconItemTypes = map[string]bool{"CollectionFolder": true, "Movie": true, "Series": true, "Season": true, "Episode": true}

// reconItems collects every captured item, merging the records of each id
// (the one with the most fields wins each key).
func reconItems(t *testing.T) map[string]jmap {
	t.Helper()
	files, _ := filepath.Glob("../../../testdata/jellyfin/*/*/[0-9]*.json")
	items := map[string]jmap{}
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case jmap:
			if id := str(n, "Id"); hex32.MatchString(id) && reconItemTypes[str(n, "Type")] && str(n, "Name") != "" {
				cur := items[id]
				if cur == nil || len(n) > len(cur) {
					for k, v := range cur {
						if _, ok := n[k]; !ok {
							n[k] = v
						}
					}
					items[id] = n
				} else {
					for k, v := range n {
						if _, ok := cur[k]; !ok {
							cur[k] = v
						}
					}
				}
			}
			for _, c := range n {
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	for _, p := range files {
		if strings.Contains(p, "/_raw/") {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Response struct{ Body any } `json:"response"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		walk(c.Response.Body)
	}
	return items
}

func toUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("recon id %q: %v", s, err)
	}
	return id
}

// seedRecon loads the recon library into the integration test database and
// makes "user1" the capture user. Call after newIntegrationServer.
func seedRecon(t *testing.T, d Deps) map[string]jmap {
	t.Helper()
	items := reconItems(t)
	execSQL(t, `UPDATE users SET id = $1 WHERE name = 'user1'`, captureUserID)

	// Libraries: one per captured CollectionFolder, plus defaults.
	folderFor := map[string]uuid.UUID{} // collection type → folder id
	libOf := map[uuid.UUID]uuid.UUID{}  // folder → library
	addLibrary := func(folder uuid.UUID, name, kind string) {
		lib := uuid.New()
		execSQL(t, `INSERT INTO libraries (id, name, kind, paths) VALUES ($1, $2, $3, $4)`, lib, name, kind, []string{"/media/" + name})
		execSQL(t, `INSERT INTO items (id, library_id, type, name, sort_name, source_kind) VALUES ($1, $2, 'CollectionFolder', $3, lower($3), 'virtual')`, folder, lib, name)
		folderFor[kind], libOf[folder] = folder, lib
	}
	for id, it := range items {
		if str(it, "Type") == "CollectionFolder" && (str(it, "CollectionType") == "movies" || str(it, "CollectionType") == "tvshows") {
			if _, dup := folderFor[str(it, "CollectionType")]; !dup {
				addLibrary(toUUID(t, id), str(it, "Name"), str(it, "CollectionType"))
			}
		}
	}
	for kind, name := range map[string]string{"movies": "Movies", "tvshows": "Shows"} {
		if _, ok := folderFor[kind]; !ok {
			addLibrary(uuid.New(), name, kind)
		}
	}

	// Seasons that episodes name but no capture listed.
	for _, it := range items {
		if str(it, "Type") != "Episode" {
			continue
		}
		if sid := str(it, "SeasonId"); sid != "" && items[sid] == nil {
			items[sid] = jmap{"Id": sid, "Type": "Season", "Name": str(it, "SeasonName"), "SeriesId": str(it, "SeriesId"), "IndexNumber": it["ParentIndexNumber"]}
		}
	}

	insert := func(id string, it jmap, parent, top uuid.UUID) {
		typ := str(it, "Type")
		var tagline *string
		if tl, ok := it["Taglines"].([]any); ok && len(tl) > 0 {
			s, _ := tl[0].(string)
			tagline = &s
		}
		prov, _ := json.Marshal(it["ProviderIds"])
		if string(prov) == "null" {
			prov = []byte("{}")
		}
		sortName := strings.ToLower(str(it, "Name"))
		if s := str(it, "SortName"); s != "" {
			sortName = s
		}
		execSQL(t, `INSERT INTO items (id, library_id, parent_id, top_parent_id, type, name, original_title, sort_name,
			index_number, parent_index_number, production_year, premiere_date, end_date, overview, tagline,
			official_rating, community_rating, critic_rating, runtime_ticks, provider_ids, source_kind, path,
			date_created, metadata_source, original_language)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,'file',$21,
			-- Items no capture carried a DateCreated for (episodes 2-5) get the
			-- scan date of episode 1, so date-ordered replays keep Jellyfin's order.
			coalesce($22, '2026-08-22T03:25:27Z'::timestamptz),'tmdb',$23)`,
			toUUID(t, id), libOf[top], parent, top, typ, str(it, "Name"), strp(it, "OriginalTitle"), sortName,
			intp(it, "IndexNumber"), intp(it, "ParentIndexNumber"), intp(it, "ProductionYear"), timep(it, "PremiereDate"), timep(it, "EndDate"),
			strp(it, "Overview"), tagline, strp(it, "OfficialRating"), nump(it, "CommunityRating"), nump(it, "CriticRating"),
			intp(it, "RunTimeTicks"), prov, strp(it, "Path"), timep(it, "DateCreated"), strp(it, "OriginalLanguage"))
	}
	byType := func(typ string) []string {
		var ids []string
		for id, it := range items {
			if str(it, "Type") == typ {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids
	}
	movies, shows := folderFor["movies"], folderFor["tvshows"]
	for _, id := range byType("Movie") {
		insert(id, items[id], movies, movies)
	}
	for _, id := range byType("Series") {
		insert(id, items[id], shows, shows)
	}
	for _, id := range byType("Season") {
		parent := shows
		if s := str(items[id], "SeriesId"); items[s] != nil {
			parent = toUUID(t, s)
		}
		insert(id, items[id], parent, shows)
	}
	for _, id := range byType("Episode") {
		parent := toUUID(t, str(items[id], "SeasonId"))
		insert(id, items[id], parent, shows)
	}

	image := func(item string, typ string, idx int, tag string, hashes jmap) {
		var hash *string
		if byTag, ok := hashes[typ].(jmap); ok {
			hash = strp(byTag, tag)
		}
		execSQL(t, `INSERT INTO images (item_id, type, idx, tag, blurhash, source_url) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			toUUID(t, item), typ, idx, tag, hash, "https://images.example/"+tag)
	}
	for id, it := range items {
		if str(it, "Type") == "CollectionFolder" {
			continue
		}
		hashes, _ := it["ImageBlurHashes"].(jmap)
		if tags, ok := it["ImageTags"].(jmap); ok {
			for typ, tag := range tags {
				image(id, typ, 0, tag.(string), hashes)
			}
		}
		if bs, ok := it["BackdropImageTags"].([]any); ok {
			for i, tag := range bs {
				image(id, "Backdrop", i, tag.(string), hashes)
			}
		}
		// Episodes describe their series' and season's artwork.
		if series := str(it, "SeriesId"); items[series] != nil {
			for typ, key := range map[string]string{"Primary": "SeriesPrimaryImageTag", "Logo": "ParentLogoImageTag", "Thumb": "ParentThumbImageTag"} {
				if tag := str(it, key); tag != "" {
					image(series, typ, 0, tag, hashes)
				}
			}
			if bs, ok := it["ParentBackdropImageTags"].([]any); ok {
				for i, tag := range bs {
					image(series, "Backdrop", i, tag.(string), hashes)
				}
			}
		}
		if p := str(it, "ParentPrimaryImageItemId"); items[p] != nil && str(it, "ParentPrimaryImageTag") != "" {
			image(p, "Primary", 0, str(it, "ParentPrimaryImageTag"), hashes)
		}

		if srcs := objs(it, "MediaSources"); len(srcs) > 0 {
			for _, src := range srcs {
				seedMediaSource(t, toUUID(t, id), src)
			}
		} else if c := str(it, "Container"); c != "" {
			var ms uuid.UUID
			if err := testPool.QueryRow(t.Context(), `INSERT INTO media_sources (item_id, name, container, path_or_url, protocol)
				VALUES ($1, $2, $3, coalesce($4, ''), 'File') RETURNING id`, toUUID(t, id), str(it, "Name"), c, strp(it, "Path")).Scan(&ms); err != nil {
				t.Fatal(err)
			}
			execSQL(t, `INSERT INTO media_streams (media_source_id, idx, type, codec, width, height) VALUES ($1, 0, 'Video', 'h264', $2, $3)`,
				ms, intp(it, "Width"), intp(it, "Height"))
			if b, _ := it["HasSubtitles"].(bool); b {
				execSQL(t, `INSERT INTO media_streams (media_source_id, idx, type, codec) VALUES ($1, 1, 'Subtitle', 'subrip')`, ms)
			}
		}
		for i, ch := range objs(it, "Chapters") {
			execSQL(t, `INSERT INTO chapters (item_id, idx, start_ticks, name) VALUES ($1,$2,$3,$4)`, toUUID(t, id), i, intp(ch, "StartPositionTicks"), strp(ch, "Name"))
		}
		for i, g := range objs(it, "GenreItems") {
			execSQL(t, `INSERT INTO genres (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, toUUID(t, str(g, "Id")), str(g, "Name"))
			execSQL(t, `INSERT INTO item_genres (item_id, genre_id, sort_order) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, toUUID(t, id), toUUID(t, str(g, "Id")), i)
		}
		for i, s := range objs(it, "Studios") {
			execSQL(t, `INSERT INTO studios (id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, toUUID(t, str(s, "Id")), str(s, "Name"))
			execSQL(t, `INSERT INTO item_studios (item_id, studio_id, sort_order) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, toUUID(t, id), toUUID(t, str(s, "Id")), i)
		}
		for i, p := range objs(it, "People") {
			var img *string
			if tag := str(p, "PrimaryImageTag"); tag != "" {
				img = ptr("https://images.example/person/" + tag)
			}
			execSQL(t, `INSERT INTO people (id, name, image_url) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, toUUID(t, str(p, "Id")), str(p, "Name"), img)
			execSQL(t, `INSERT INTO item_people (item_id, person_id, kind, role, sort_order) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
				toUUID(t, id), toUUID(t, str(p, "Id")), str(p, "Type"), strp(p, "Role"), i)
		}
		if ud, ok := it["UserData"].(jmap); ok && str(it, "Type") != "CollectionFolder" {
			played, _ := ud["Played"].(bool)
			fav, _ := ud["IsFavorite"].(bool)
			execSQL(t, `INSERT INTO user_data (user_id, item_id, played, play_count, playback_position_ticks, is_favorite, last_played_at)
				VALUES ($1,$2,$3,coalesce($4,0),coalesce($5,0),$6,$7)`, captureUserID, toUUID(t, id), played, intp(ud, "PlayCount"),
				intp(ud, "PlaybackPositionTicks"), fav, timep(ud, "LastPlayedDate"))
		}
	}
	return items
}

// seedMediaSource stores a captured MediaSourceInfo and its streams.
func seedMediaSource(t *testing.T, item uuid.UUID, src jmap) {
	t.Helper()
	var ms uuid.UUID
	if err := testPool.QueryRow(t.Context(), `INSERT INTO media_sources (item_id, name, container, size, bitrate, path_or_url, protocol, is_remote, etag, runtime_ticks)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, item, str(src, "Name"), strp(src, "Container"), intp(src, "Size"),
		intp(src, "Bitrate"), str(src, "Path"), str(src, "Protocol"), src["IsRemote"] == true, strp(src, "ETag"), intp(src, "RunTimeTicks")).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	b := func(m jmap, k string) bool { v, _ := m[k].(bool); return v }
	for _, st := range objs(src, "MediaStreams") {
		execSQL(t, `INSERT INTO media_streams (media_source_id, idx, type, codec, language, title, is_default, is_forced, is_external,
			is_hearing_impaired, is_original, width, height, bitrate, channels, channel_layout, sample_rate, video_range, video_range_type,
			profile, level, pixel_format, bit_depth, aspect_ratio, average_frame_rate, real_frame_rate, is_interlaced,
			color_transfer, color_primaries, color_space, color_range, time_base, dv_profile, dv_level, dv_bl_compat_id,
			dv_version_major, dv_version_minor, dv_rpu_present, dv_el_present, dv_bl_present)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,
			$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40)`,
			ms, intp(st, "Index"), str(st, "Type"), strp(st, "Codec"), strp(st, "Language"), strp(st, "Title"),
			b(st, "IsDefault"), b(st, "IsForced"), b(st, "IsExternal"), b(st, "IsHearingImpaired"), b(st, "IsOriginal"),
			intp(st, "Width"), intp(st, "Height"), intp(st, "BitRate"), intp(st, "Channels"), strp(st, "ChannelLayout"),
			intp(st, "SampleRate"), strp(st, "VideoRange"), strp(st, "VideoRangeType"), strp(st, "Profile"), nump(st, "Level"),
			strp(st, "PixelFormat"), intp(st, "BitDepth"), strp(st, "AspectRatio"), nump(st, "AverageFrameRate"),
			nump(st, "RealFrameRate"), b(st, "IsInterlaced"),
			strp(st, "ColorTransfer"), strp(st, "ColorPrimaries"), strp(st, "ColorSpace"), strp(st, "ColorRange"), strp(st, "TimeBase"),
			intp(st, "DvProfile"), intp(st, "DvLevel"), intp(st, "DvBlSignalCompatibilityId"), intp(st, "DvVersionMajor"),
			intp(st, "DvVersionMinor"), flagp(st, "RpuPresentFlag"), flagp(st, "ElPresentFlag"), flagp(st, "BlPresentFlag"))
	}
}

// flagp reads Jellyfin's 0/1 Dolby Vision flags.
func flagp(m jmap, k string) *bool {
	if f, ok := m[k].(float64); ok {
		b := f != 0
		return &b
	}
	return nil
}
