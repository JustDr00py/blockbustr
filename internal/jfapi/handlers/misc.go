package handlers

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"unicode/utf16"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sysadmin/blockbustr/internal/auth"
	"github.com/sysadmin/blockbustr/internal/jfapi"
	"github.com/sysadmin/blockbustr/internal/jfapi/dto"
	"github.com/sysadmin/blockbustr/internal/store/pg/db"
)

// Endpoints clients call for features blockbustr doesn't have (yet), answered
// with the valid empty result Jellyfin 12.1.0 sends for an item without them
// (TASKS P1.23, DESIGN §3.10), plus stored display preferences.

func (a *api) registerMisc(rt *jfapi.Router) {
	emptyResult := a.requireUser(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		jfapi.WriteJSON(w, r, http.StatusOK, queryResult([]dto.BaseItemDto{}))
	})
	for _, p := range []string{
		"/Items/{itemId}/Collections", "/Items/{itemId}/Intros", "/Users/{userId}/Items/{itemId}/Intros",
		"/MediaSegments/{itemId}", "/LiveTv/Programs/Recommended",
	} {
		rt.Get(p, emptyResult)
	}
	emptyList := a.requireUser(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		jfapi.WriteJSON(w, r, http.StatusOK, []any{})
	})
	for _, p := range []string{
		// Bare arrays of BaseItemDto.
		"/Items/{itemId}/SpecialFeatures", "/Users/{userId}/Items/{itemId}/SpecialFeatures",
		"/Items/{itemId}/LocalTrailers", "/Users/{userId}/Items/{itemId}/LocalTrailers",
		"/SyncPlay/List", "/web/ConfigurationPages",
		"/Localization/Cultures", "/Localization/Countries", "/Localization/ParentalRatings", "/Localization/Options",
	} {
		rt.Get(p, emptyList)
	}
	adminEmptyList := a.requireAdmin(func(w http.ResponseWriter, r *http.Request, _ auth.Session) {
		jfapi.WriteJSON(w, r, http.StatusOK, []any{})
	})
	for _, p := range []string{"/Plugins", "/Packages", "/ScheduledTasks"} {
		rt.Get(p, adminEmptyList)
	}
	rt.Get("/Items/{itemId}/ThemeMedia", a.requireUser(a.themeMedia))
	rt.Get("/Items/{itemId}/ThemeSongs", a.requireUser(a.themeResult))
	rt.Get("/Items/{itemId}/ThemeVideos", a.requireUser(a.themeResult))
	rt.Get("/DisplayPreferences/{prefId}", a.requireUser(a.getDisplayPreferences))
	rt.Post("/DisplayPreferences/{prefId}", a.requireUser(a.setDisplayPreferences))
}

// emptyTheme is a ThemeMediaResult with no items, owned by owner.
func emptyTheme(owner dto.ID) *dto.ThemeMediaResult {
	return &dto.ThemeMediaResult{OwnerId: &owner, Items: &[]dto.BaseItemDto{}, TotalRecordCount: ptr(int32(0)), StartIndex: ptr(int32(0))}
}

// themeOwner is the item a theme request names (the zero id if unparsable).
func themeOwner(r *http.Request) dto.ID {
	id, _ := dto.ParseID(jfapi.URLParam(r, "itemId"))
	return id
}

// themeMedia answers like Jellyfin for an item without theme media: theme
// songs and videos owned by the item, soundtracks by nobody.
func (a *api) themeMedia(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	owner := themeOwner(r)
	jfapi.WriteJSON(w, r, http.StatusOK, dto.AllThemeMediaResult{
		ThemeVideosResult: emptyTheme(owner), ThemeSongsResult: emptyTheme(owner), SoundtrackSongsResult: emptyTheme(dto.ID{}),
	})
}

func (a *api) themeResult(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	jfapi.WriteJSON(w, r, http.StatusOK, emptyTheme(themeOwner(r)))
}

// displayPrefsID is Jellyfin's Id for a preferences key: the MD5 of the
// key's UTF-16LE bytes as a .NET Guid, dashed ("usersettings" →
// 3ce5b65d-e116-d731-65d1-efc4a30ec35c, observed).
func displayPrefsID(key string) string {
	units := utf16.Encode([]rune(key))
	buf := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[2*i:], u)
	}
	sum := md5.Sum(buf)
	// .NET stores the first three Guid fields little-endian.
	g := uuid.UUID(sum)
	g[0], g[1], g[2], g[3] = sum[3], sum[2], sum[1], sum[0]
	g[4], g[5] = sum[5], sum[4]
	g[6], g[7] = sum[7], sum[6]
	return g.String()
}

// defaultDisplayPreferences is what 12.1.0 returns for a key nobody saved.
func defaultDisplayPreferences() dto.DisplayPreferencesDto {
	return dto.DisplayPreferencesDto{
		SortBy: ptr("SortName"), SortOrder: ptr(dto.Ascending), RememberIndexing: ptr(false), RememberSorting: ptr(false),
		PrimaryImageHeight: ptr(int32(250)), PrimaryImageWidth: ptr(int32(250)), CustomPrefs: &map[string]*string{},
		ScrollDirection: ptr(dto.Horizontal), ShowBackdrop: ptr(true), ShowSidebar: ptr(false),
	}
}

// prefsKey is the (user, key, client) a preferences request addresses.
func prefsKey(r *http.Request, s auth.Session) (user uuid.UUID, key, client string, ok bool) {
	user, ok = queryUser(r, s)
	client = jfapi.QueryOf(r).Get("client")
	if client == "" {
		client = "emby" // what jellyfin-web sends
	}
	return user, jfapi.URLParam(r, "prefId"), client, ok
}

func (a *api) getDisplayPreferences(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, key, client, ok := prefsKey(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	prefs := defaultDisplayPreferences()
	raw, err := a.Queries.GetDisplayPreferences(r.Context(), db.GetDisplayPreferencesParams{UserID: user, PrefID: key, Client: client})
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &prefs); err != nil {
			a.internalError(w, r, err)
			return
		}
	case !errors.Is(err, pgx.ErrNoRows):
		a.internalError(w, r, err)
		return
	}
	prefs.Id, prefs.Client = ptr(displayPrefsID(key)), &client
	jfapi.WriteJSON(w, r, http.StatusOK, prefs)
}

// setDisplayPreferences stores the posted preferences (unset fields keep
// their defaults) and answers 204, like Jellyfin.
func (a *api) setDisplayPreferences(w http.ResponseWriter, r *http.Request, s auth.Session) {
	user, key, client, ok := prefsKey(r, s)
	if !ok {
		errorText(w, http.StatusForbidden)
		return
	}
	prefs := defaultDisplayPreferences()
	if err := jfapi.DecodeJSON(w, r, &prefs); err != nil {
		errorText(w, http.StatusBadRequest)
		return
	}
	prefs.Id, prefs.Client = nil, nil // derived on read
	data, err := json.Marshal(prefs)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if err := a.Queries.UpsertDisplayPreferences(r.Context(), db.UpsertDisplayPreferencesParams{UserID: user, PrefID: key, Client: client, Data: data}); err != nil {
		a.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
