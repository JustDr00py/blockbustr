# DESIGN.md — blockbustr technical design

Status: **draft v0.2** (2026-10-05). This file is the source of truth. Change it before you change direction in code.

---

## 1. Goals
1. Unmodified Jellyfin native clients work: Findroid, Jellyfin Android / Android TV, Swiftfin, Streamyfin, Infuse, Jellyfin for Kodi.
2. Fast at scale. A library of 50k items should give `/Items` p95 < 50 ms with Redis warm, and a scan should never block browsing.
3. Remote media is first-class: `.strm`, debrid, and Stremio addon streams appear as normal items.
4. **In-client search finds anything (§7.4).** The search box in any Jellyfin app returns library matches plus movies and shows from Stremio addons/TMDB that you don't have yet. Picking one plays it through Stremio streams + debrid. No plugin page or separate web UI is needed (unlike jellybird).
5. A single Go binary, plus Postgres and Redis. Docker-first.

## 2. Architecture

```
 Jellyfin clients (Findroid, Swiftfin, Infuse, Kodi, …)
            │  HTTP + WebSocket (/socket)
            ▼
 ┌──────────────────────────── blockbustr ─────────────────────────────┐
 │ jfapi  (router, case-insensitive mw, auth, DTOs, handlers)          │
 │   │                                                                 │
 │   ├─ library ── scanner ── strm / naming parser ── media (ffprobe)  │
 │   ├─ catalog ── stremio (addon client, registry) ── metadata/tmdb   │
 │   ├─ playback ─ media.decide ── transcode (HLS/remux) ── resolve    │
 │   │                                         │                       │
 │   │                                         └─ provider (RD/TorBox) │
 │   ├─ userdata / sessions ── events (pub/sub) ── websocket hub       │
 │   └─ images (fetch/resize/cache)                                    │
 │ store/pg (sqlc) ────────────── cache (redis)                        │
 └─────────────┬──────────────────────────────┬────────────────────────┘
               ▼                              ▼
          PostgreSQL 17                    Redis 8
```

### Repo layout
```
cmd/blockbustr/main.go
internal/
  config/                  YAML + env
  jfapi/
    router.go              chi; case-insensitive path + query middleware
    authmw.go              MediaBrowser header parsing → ctx(User, Device, Session)
    dto/                   generated (oapi-codegen types) + small hand helpers (ids, ticks, time)
    handlers/              system.go users.go views.go items.go shows.go userdata.go
                           playback.go videos.go hls.go subtitles.go images.go
                           sessions.go socket.go search.go misc.go (Localization, DisplayPreferences, Plugins…)
  auth/                    password hashing, token issue/revoke, QuickConnect
  library/                 libraries, scanner, item hierarchy builder, watcher (fsnotify)
  strm/                    release-name parsing, .strm read/write   (ported from jellybird)
  media/                   ffprobe wrapper, DeviceProfile → decision (ported from jollyrogarr)
  transcode/               ffmpeg HLS sessions, hwaccel detection    (ported from jollyrogarr)
  stremio/                 addon protocol client, registry, catalog sync, stream ranking
  provider/                debrid Provider interface + realdebrid/ torbox/ (ported from jellybird)
  resolve/                 Source → playable URL, Redis-cached
  metadata/tmdb/           TMDB client                               (ported from jellybird)
  images/                  remote fetch, resize, disk cache, blurhash
  events/                  typed events, Redis pub/sub, WebSocket hub
  store/pg/                migrations/ (goose), queries/ (sqlc), generated code
  cache/                   Redis helpers (keys, TTLs, locks)
testdata/jellyfin/         scrubbed request/response fixtures from real Jellyfin
scripts/                   gen-dto, scrub-fixtures, capture (mitmproxy)
deploy/docker-compose.yml
```

Rule: `jfapi` depends on the domain packages, never the other way round. Domain packages expose plain Go types, and `jfapi` maps them to DTOs.

## 3. Jellyfin compatibility layer

### 3.1 Wire conventions
| Thing | Rule |
|---|---|
| JSON keys | **PascalCase by default** for every endpoint (no Accept, `*/*`, `application/json`, …). **camelCase only** when the first JSON-capable Accept range (in descending q order) carries `profile=CamelCase`. **Dictionary keys are never renamed** (`ImageTags`, `ProviderIds`, `ImageBlurHashes`…; generated list `dto.MapProperties`). Content-Type echoes the profile only when one was requested. Verified on the recon server, plus golden camel/Pascal pairs in `internal/jfapi/testdata/casing`. *(The camelCase seen in P0.1 came from a fresh server before the setup wizard finished.)* |
| IDs | uuid in PG, `dto.ID` on the wire: every spec field with `format: uuid` is 32 lower-case hex (N). Input accepts N/dashed/braced/upper-case. Dashed GUID *text* also appears in plain string fields (`UserData.Key`, client `DeviceId`), so don't reformat those |
| Time | `dto.Time`: UTC + `Z`, 100 ns. Non-zero fractions are trimmed (`…34.529331Z`), zero fractions are `.0000000Z` (verified on 851 captured responses) |
| Duration / position | ticks (int64, 100 ns) |
| Paging | `StartIndex`, `Limit` → `{ "Items": [...], "TotalRecordCount": N, "StartIndex": S }` |
| `Fields` param | comma list; optional heavy fields (Overview, People, MediaSources, Chapters, Genres…) are only filled when asked for |
| Errors | auth failure → 401; otherwise prefer an empty valid result to 4xx/5xx |

### 3.2 Auth
- **Observed on 12.1.0:** `EnableLegacyAuthorization` defaults to **false**. Jellyfin then accepts only `Authorization: MediaBrowser …` and `?ApiKey=`, and **rejects** (401) `X-Emby-Authorization`, `X-Emby-Token`, `X-MediaBrowser-Token` and `?api_key=`. Every captured client uses the accepted forms.
- blockbustr accepts the legacy forms too, controlled by `compat.legacy_auth` (default **true**), because older clients (some Kodi/Infuse builds) still send them, and accepting more forms costs nothing in compatibility.
- **Implemented (P1.8):** `jfapi.ParseAuth(r, legacy)` → `AuthInfo{Client, Device, DeviceID, Version, Token, TokenSource}`. A router middleware stores it in the context (`jfapi.AuthFrom(ctx)`); resolving the token to a user is P1.10.
  - Client fields come from `Authorization: MediaBrowser …`, falling back to legacy `X-Emby-Authorization` (and the legacy `Emby` scheme).
  - The token is the first real one from: header `Token` → legacy `X-Emby-Token` → legacy `X-MediaBrowser-Token` → `?ApiKey=` → legacy `?api_key=`.
  - **Placeholder tokens are ignored**: Jellyfin for Android sends `Token="null"` and Streamyfin sends `Token=""` before login, so a real `?ApiKey=` still counts.
  - Values are URL-decoded (clients send both `Jellyfin%20for%20Android` and `Jellyfin+for+Android`). Quoted values may contain commas and `\"`. Scheme and keys are case-insensitive, and the first duplicate wins. DeviceIds are opaque strings (Streamyfin's is a dashed GUID).
  - Verified against all 1,227 captured requests.
- Fields: `Client`, `Device`, `DeviceId`, `Version`, `Token`. Values may be URL-encoded.
- `POST /Users/AuthenticateByName` `{Username, Pw}` → `AuthenticationResult {User, SessionInfo, AccessToken, ServerId}`.
- **Implemented (P1.10):** `internal/auth` (domain) + `handlers/users.go`, `authmw.go`. Observed 12.1.0 behaviour that is copied:
  - Login without client/device fields → **400**; bad user or password → **401**. Both have a `text/plain` body of `Error processing request.`.
  - A protected endpoint with no or invalid token → **bare 401** (no body, no Content-Type).
  - After login, a token-only header (`MediaBrowser Token="…"`) or `?ApiKey=` is enough.
  - Usernames are case-insensitive. A passwordless user signs in with an empty `Pw`.
  - **Logging in again on a device revokes that device's earlier tokens**, in Postgres and in the Redis token cache. A `devtok:{deviceId}` set records which cached token hashes belong to the device, so a revoked token never survives in cache.
  - `UserDto.Configuration`/`Policy` = Jellyfin's defaults (embedded from a real response, `handlers/defaults/*.json`), then `users.configuration`/`users.policy` overrides, then `is_admin`/`is_disabled` from the row. Default `IsHidden: true` means `/Users/Public` is `[]`, as in 12.1.0. *Open:* verify non-admin default policy values against a non-admin created on Jellyfin (P4.1).
  - The admin is bootstrapped from `server.admin_username/admin_password` (env `BLOCKBUSTR_ADMIN_*`): created if missing, otherwise password reset and made an enabled admin (recovery). bcrypt hashes; >72-byte passwords are rejected.
  - Endpoints: `/Users/Me`, `/Users/{id}` (self or admin, otherwise 403), `/Users` (admin), `/Users/Public`, `POST /Sessions/Logout` (204), `/System/Info`, `/System/Endpoint` (IsLocal = loopback/private/link-local/CGNAT), `/QuickConnect/Enabled` → `false` until P2.12.
  - SessionInfo has a stable per-device `Id` (UUIDv5 of the DeviceId); live session state is P2.9.
- A token is 32 random hex chars. It is stored hashed (sha256) in PG and cached in Redis `tok:{sha}` → `{userId, deviceId}` for 24h (sliding).
- QuickConnect: `/QuickConnect/Enabled|Initiate|Connect|Authorize`, with state in Redis (`qc:{secret}`, 10 min TTL).

### 3.1a HTTP layer (implemented, P1.7: `internal/jfapi`)
- **Case-insensitive routing.** `Router` wraps chi. A canonicalizer built from the registered patterns rewrites only the *literal* parts of the request path to their registered casing before chi routes it (`/items/x/images/Primary` → `/Items/x/Images/Primary`). Parameter values, including in-segment ones such as `stream.{container}` and `{segmentId}.{ext}`, keep their original text. Literal segments beat parameters (`/Items/Latest` before `/Items/{itemId}`), and `*` wildcards are supported. Unregistered paths fall through to 404.
- **JSON out:** `WriteJSON(w, r, status, dto)` → `json.Marshal`, then `camelJSON` (the .NET `JsonNamingPolicy.CamelCase` algorithm on property names, dictionary keys kept, key order and numbers preserved) when negotiated.
- **JSON in:** `DecodeJSON` uses Go's case-insensitive field matching, so camelCase bodies (Streamyfin) decode into the PascalCase DTOs. 10 MB cap. An empty body leaves the target unchanged.
- **Query:** `QueryOf(r)`: case-insensitive names, values in URL order across casings, comma lists split, `Bool`/`Int` helpers.
- **Middleware:** panic → logged 500. One log line per request, with the **canonical path only and never the query** (tokens live in `?ApiKey=`); 2xx/3xx at debug because clients poll. CORS like Jellyfin: `Access-Control-Allow-Origin: *` everywhere, and preflight → 204 echoing the requested method and headers.

### 3.1b Library views (implemented, P1.18)
- `/UserViews` and legacy `/Users/{id}/Views` return each **enabled** library's CollectionFolder with Jellyfin's full field set: always-present empty lists, `ChannelId: null`, `PlayAccess: Full`, `DateLastMediaAdded: 0001-01-01…`, `ChildCount` = visible direct children.
  - `UserData.Key` is the dashed item GUID. `ParentId` is a stable root-folder id (UUIDv5 of the server id).
  - The library order is the sort name. Per-user library access and ordering come in P4.1.
- `/Library/VirtualFolders` and `/Library/MediaFolders` are admin-only. VirtualFolders carries Jellyfin's default `LibraryOptions` for the kind, with `PathInfos` from the configured paths. Jellyfin's built-in "Playlists" folder is not listed (no playlists yet).
- **Libraries removed from `config.yaml` are disabled, not deleted** (`libraries.enabled`), so their items and users' history survive a mistaken edit.

### 3.3 DTO generation (implemented, P1.6)
- `internal/jfapi/dto/openapi.json` is the pinned 12.1.0 spec (OpenAPI 3.0.4, 357 schemas, 294 paths), fetched from the recon server's `/api-docs/openapi.json` and stored unmodified.
- `scripts/gen-dto.py` (`make dto`) preprocesses it and runs oapi-codegen v2.8 (models only, `skip-prune`) → `types.gen.go` (~17.5k lines):
  - `format: uuid` → `dto.ID`, `format: date-time` → `dto.Time`
  - `BaseItemDto.ChannelId` is marked `x-omitempty: false`, because Jellyfin always sends it, even as `null`.
  - `ImageBlurHashes` (BaseItemDto, BaseItemPerson) is rewritten from the spec's per-ImageType inline object to `map[type]map[tag]hash`, the C# Dictionary it really is (same JSON).
  - `make dto-check` fails on drift.
- Optional fields are pointers with `omitempty`, matching Jellyfin's omit-nulls behaviour. Optional collections are `*[]T`, so "absent" and `[]` stay distinct.
- `TestFixturesRoundTrip` decodes every captured 200 response for 21 endpoints (851 responses) with unknown fields disallowed, re-encodes it, and requires the identical JSON value.
- `BaseItemDto` builders (domain → DTO) come with the endpoints that need them (P1.20).

### 3.4 Reported identity (implemented, P1.9)
- `Id` is generated once and kept in `server_settings.server_id` (`pg.EnsureServerID`, first writer wins), so it survives restarts and reinstalls that keep the database. Clients key saved servers on it.
- `LocalAddress` is `server.external_url` if set, otherwise the scheme and host the request arrived on (honouring `X-Forwarded-Proto: https`).
- `OperatingSystem` is sent as `""`, like 12.1.0.
- `/System/Ping` (GET/POST) returns `compat.product_name` as a JSON string. `/Branding/Configuration` returns `{"SplashscreenEnabled":false}` and `/Branding/Css` an empty `text/css` body, until the admin UI makes them configurable.

Original sketch:
`/System/Info/Public`: `ServerName`, `Version: "12.1.0"` (pinned; see Q1), `ProductName: "Jellyfin Server"` (some clients check it; see Q2), `Id` (stable server uuid), `StartupWizardCompleted: true`, `LocalAddress`.

### 3.5 MVP endpoints (grouped)
- **System:** `GET /System/Info/Public`, `/System/Info`, `/System/Ping` (GET/POST), `/System/Endpoint`, `/Branding/Configuration`, `/Branding/Css`
- **Users/Auth:** `POST /Users/AuthenticateByName`, `GET /Users/Me`, `/Users/Public`, `/Users/{id}`, `POST /Sessions/Logout`, QuickConnect `GET /QuickConnect/Enabled`, `/QuickConnect/Initiate|Connect|Authorize` (§3.2), `POST /Users/AuthenticateWithQuickConnect`
- **Views:** `GET /UserViews` (+ legacy `/Users/{id}/Views`), `/UserViews/GroupingOptions`
- **Items:** `GET /Items` (+ `/Users/{id}/Items`), `/Items/{id}` (+ `/Users/{id}/Items/{id}`), `/Items/Latest`, `/UserItems/Resume`, `/Items/{id}/Similar`, `/Items/{id}/Ancestors`, `/Items/Filters`, `/Items/Filters2`, `/Items/{id}/ThemeMedia` (empty), `/Items/{id}/SpecialFeatures` (empty), `/Items/{id}/LocalTrailers` (empty)
- **Shows:** `/Shows/NextUp`, `/Shows/{id}/Seasons`, `/Shows/{id}/Episodes`
- **Seen in Jellyfin for Android 2.7.3 (jellyfin-web in a WebView + native Media3 player):**
  - static: `GET /` (redirects to `/web/`), `GET /web/{file}` (the jellyfin-web bundle, Phase 5), `GET /web/config.json`
  - `GET /Playback/BitrateTest` (bandwidth probe: returns N random bytes; `?Size=`), `GET /Users/{id}/Items/{id}/Intros` → `{Items:[]}`, `GET /Items/{id}/Collections` → `[]`-style empty result, `GET /LiveTv/Programs/Recommended` → `{Items:[]}`, `GET /SyncPlay/List` → `[]`
  - QuickConnect: the app polls `GET /QuickConnect/Connect?Secret=` about every 5s while the code is shown
  - **Search** doesn't use `/Search/Hints`. It fires three parallel `GET /Items?searchTerm=…&recursive=true` calls split by type (`excludeItemTypes=Movie,Episode,TvChannel&mediaTypes=Video`; `includeItemTypes=LiveTvProgram`; then Movie/Series/Episode/BoxSet/… with `limit=800`) plus `GET /Artists?searchTerm=` → `{Items:[]}`. The `/Items` filter builder must support `searchTerm`, `includeItemTypes`, `excludeItemTypes`, `mediaTypes`, `isMissing` and `enableTotalRecordCount=false` (omit the count)
  - **Favourites** use the legacy `POST/DELETE /Users/{id}/FavoriteItems/{id}`, which return the updated `UserItemDataDto` (`IsFavorite`, `PlayCount`, `PlaybackPositionTicks`, `LastPlayedDate`, …) with 200, not 204
- **Client plugin probes:** `GET /Streamyfin/config` must return **404** (that's what Jellyfin without the Streamyfin companion plugin returns; Streamyfin treats 404 as "plugin not installed"). Never answer unknown plugin routes with an empty 200.
- **Seen in Findroid capture, missing before:** `/Items/Suggestions`, `/MediaSegments/{id}` (intro/credits segments; return `{Items:[]}` until Phase 5). Image paths arrive as lowercase `/items/{id}/Images/...`, which confirms the case-insensitive routing
- **Browse aux:** `/Genres`, `/Studios`, `/Persons`, `/Search/Hints`, `/Library/VirtualFolders`, `/Library/MediaFolders`
- **User data:** `POST/DELETE /UserPlayedItems/{id}`, `POST/DELETE /UserFavoriteItems/{id}`, legacy `/Users/{u}/PlayedItems/{id}`, `/Users/{u}/FavoriteItems/{id}`, `POST /UserItems/{id}/UserData`
- **Playback:** `POST|GET /Items/{id}/PlaybackInfo`, `GET|HEAD /Videos/{id}/stream[.{container}]`, `/Videos/{id}/master.m3u8`, `/Videos/{id}/main.m3u8`, `/Videos/{id}/hls1/{playlistId}/{segmentId}.{ext}`, `DELETE /Videos/ActiveEncodings`, `/Videos/{id}/{msId}/Subtitles/{idx}/[{startTicks}/]Stream.{fmt}`
- **Sessions:** `POST /Sessions/Capabilities[/Full]`, `/Sessions/Playing`, `/Sessions/Playing/Progress`, `/Sessions/Playing/Stopped`, `/Sessions/Playing/Ping`, `GET /Sessions`, WebSocket `/socket`
- **Images:** `GET|HEAD /Items/{id}/Images/{type}[/{index}]` with `maxWidth,maxHeight,fillWidth,fillHeight,quality,tag,format`
- **Misc:** `/DisplayPreferences/{key}` (GET/POST, stored per user+client; keys like `usersettings`), `/Localization/Cultures|Countries|ParentalRatings|Options`, `/Plugins` (`[]`), `/Packages` (`[]`), `/ScheduledTasks` (`[]`), `/web/ConfigurationPages` (`[]`)
- **Kodi (Phase 4):** Kodi Sync Queue plugin endpoints `/Jellyfin.Plugin.KodiSyncQueue/{UserId}/GetItems?LastUpdateDT=…`

### 3.6 Item queries and BaseItemDto (implemented, P1.19)
- **`/Items` and legacy `/Users/{id}/Items`** map onto `pg.ItemQuery` (`internal/store/pg/itemquery.go`). It builds parameterised SQL, so every value is a bind argument.
  - **Filters:** parentId (+ recursive), include/exclude types, mediaTypes, ids, excludeItemIds, personIds, genreIds, `|`-separated genres and officialRatings, years, anyProviderIdEquals, nameStartsWith/OrGreater/LessThan, isPlayed/isFavorite, `filters=` (IsFolder, IsPlayed, IsResumable…), minCommunityRating.
  - **Always applied:** only enabled libraries, never missing items. `isMissing=true` and an unparsable parentId return an empty result, like Jellyfin. `parentId` = the root folder means "no parent".
  - **Search:** accent-insensitive prefix full-text **or** substring match. With no explicit sort, results rank exact title, then title prefix, then trigram similarity.
  - **Sorting:** per-key `sortOrder` (missing entries take the first). Nulls compare as the minimum, as in Jellyfin. Unknown keys are ignored, and `i.id` is always the final tie-break.
  - **Counts:** `TotalRecordCount` only runs a count query when the page can't tell (a full page, or past the end). With `enableTotalRecordCount=false` it is the page length, as Jellyfin reports it.
  - **Played:** a Series or Season is Played when it has episodes and all are played.
- **DTO builder** (`handlers/itembuild.go`): a per-kind base set plus the requested `fields`. Everything is batch-loaded, about 8 queries a page whatever its size: parents, images, media summary, user data, episode counts, genres, studios, people, chapters. Rules matched from captures:
  - **Media:** `HasSubtitles` only when true. `Width`/`Height`/`IsHD` are omitted for unprobed `.strm`.
  - **UserData:** `PlayedPercentage` = position ÷ runtime for partly watched videos, and played ÷ total for a Series or Season, which also get `UnplayedItemCount`.
  - **Images:** an episode borrows the series' Primary/Backdrop/Logo/Thumb and the season's poster (`Parent*ImageTag`, `SeriesPrimaryImageTag`), and the matching blurhashes are added to its own `ImageBlurHashes`. `enableImages`, `imageTypeLimit` and `enableImageTypes` are honoured.
  - **Series:** `Status` is derived from the TMDB end date (stored only for ended or cancelled shows).
  - **`OriginalLanguage`** (ISO 639-1, from TMDB `original_language`) is part of the base set for Movie and Series on every endpoint (P1.21); seasons and episodes never carry it.
  - **Not yet:** Tags/keywords, remote trailers (`RemoteTrailers` sent empty), `ProductionLocations` (sent empty), person blurhashes.
- **Remote search hook:** `Deps.RemoteSearch` (`SearchItems(term, types, limit)`), nil until P3.12. On a search's first page, its results are appended **after** library matches, de-duplicated and within `limit`, using `remoteTypes` (Movie/Series only, honouring include/exclude/mediaTypes). A failing source is logged and never fails the search (§7.4).
- **Contract tests** seed the **recon library under its recon IDs** (`handlers/recon_test.go`, from every captured response), so all captured `/Items` requests replay unchanged.
  - They return the same items in the same order as Jellyfin (random sorts are compared as sets), with the same JSON shape.
  - Ignored paths are documented in `items_test.go`.

### 3.7 Item details and media sources (implemented, P1.20)
- **`/Items/{id}` and legacy `/Users/{userId}/Items/{id}`** use the §3.6 builder with Jellyfin's **full detail field set**, whatever `fields` says (`detailFields`).
  - Visibility is the same `pg.QueryItems` rule as browsing: enabled library, not missing. Anything else is 404.
  - Library folders use the views DTO. The root folder id gives a `UserRootFolder` ("Media Folders").
- **`/Items/{id}/Ancestors`:** parents nearest first, ending with the root folder (Season → Series → CollectionFolder → UserRootFolder).
- **MediaSources** (`handlers/mediasource.go`, reused by PlaybackInfo in P2.1):
  - The first source's `Id` is the item id, as in Jellyfin. Every `Supports*` flag is true in details; the play decision is P2's.
  - An unprobed `.strm` reports `Container: "strm"` and `Path` = its URL, as Jellyfin does.
  - **A playable item always has ≥1 source**: with no source row yet, a placeholder is built from the item (§7.4).
  - `DefaultAudioStreamIndex`: the default-flagged audio track, else the first. `DefaultSubtitleStreamIndex`: the default-flagged subtitle, else `-1`.
- **MediaStreams:**
  - **`DisplayTitle`** follows Jellyfin's rules (`internal/media/display.go`): resolution/codec/range for video; language, codec friendly name (Dolby Digital+…), layout and Default/Original for audio; Hearing Impaired/Default/Forced/codec for subtitles. A stream title is kept, with only the attributes it lacks appended. All 107 distinct captured streams match.
  - **Other fields:** `IsAVC`/`NalLengthSize` only for H.264, `RefFrames` for everything but HEVC, `Localized*` strings, text-subtitle flags and the full Dolby Vision record.
  - **`TimeBase`** comes from ffprobe. Migration 00009 added it and the remaining Dolby Vision fields, and cleared local sources' etags so each file is re-probed once.
- **Contract:** every captured `/Items/{id}`, legacy and Ancestors request matches Jellyfin's shape. Captures taken before Jellyfin probed a `.strm` are replayed against an unprobed item. The ignored paths are documented in `itemdetail_test.go`.

### 3.8 Home rows and TV endpoints (implemented, P1.21)
- **`/Items/Latest`** answers with a **bare array** (no QueryResult wrapper). One pass per library (the `parentId` one, or every enabled library), newest `date_created` first. With no `includeItemTypes`, a movies library contributes Movies and a TV library **Series** (observed; not episodes). With `groupItems=true` and Episode among the types, each episode is replaced by **its series at the episode's position** — one per series — and that entry (only) carries `ChildCount` = the series' episode count; a Series entry earlier in the list is *not* deduplicated against it, matching Jellyfin (captured: `[Season 1, MF GHOST, MF GHOST+ChildCount]`).
- **`/UserItems/Resume`:** recursive, unplayed and `playback_position_ticks > 0`, sorted `DatePlayed` descending. `excludeActiveSessions` waits for live sessions (P2.9).
- **`/Shows/NextUp`** (`pg.NextUpEpisodeIDs`): the first unplayed episode (season, number) of every series the user has **started** — some episode played, begun or re-watched. A series nobody touched is *not* next up (observed: fresh captures answered `[]`). Half-watched episodes only count when `enableResumable=true`, and then they win over later fresh ones; `enableRewatching` is accepted but inert until P2.9; `nextUpDateCutoff` drops later-airing episodes; `seriesId` narrows to one series. Ordered by the series' most recent activity. `TotalRecordCount` comes from a window count, so full pages report the real total.
- **`/Shows/{id}/Seasons`:** the series' seasons by `IndexNumber` (ParentId + Season type via `pg.ItemQuery`).
- **`/Shows/{id}/Episodes`:** aired order (`parent_index_number, index_number`), optionally narrowed to `seasonId`. **`adjacentTo`** returns the episode before, the named one and the one after (as many as exist), capped by `limit` — what clients ask around autoplay (captured: E1 → `[E1, E2]`, total 2).
- **`/Items/{id}/Similar`** (`pg.SimilarItemIDs`): **every** other Movie/Series (Jellyfin also lists items that share nothing), scored 3 per shared genre + 2 per studio + 1 per person, ties broken by the closer production year then sort name. `ProviderIds` is always sent on similar items, whatever `fields` says. Episodes/seasons get an empty result.
- **Not yet:** Redis caching of the §5 `q:latest`/`q:nextup` keys (cheap queries so far), `excludeActiveSessions` (P2.9), `enableRewatching`.

### 3.9 Browse aux: names, filters, search hints (implemented, P1.22)
- **`/Genres`, `/Studios`, `/Persons`** (`pg.QueryNames`) list only names that a **visible item uses**. The item scope is the `/Items` filter clause (`ItemQuery.from`): recursive under `parentId` (the user root means everything), plus `includeItemTypes`/`excludeItemTypes`/`mediaTypes`. Names are filtered by `searchTerm` (accent-insensitive substring), `nameStartsWith[OrGreater]`/`nameLessThan`, `personTypes`/`excludePersonTypes` and `appearsInItemId`, sorted by name (`sortOrder=Descending` reverses it), and paged.
  - **DTO (from the `/Studios` capture):** `Type` Genre/Studio/Person, `UserData.Key` = `"<Type>-<Name>"`, `ChildCount` = distinct matching items, `MovieCount`/`SeriesCount`/`EpisodeCount`, and every other count 0. `ImageTags`/`ImageBlurHashes` are always objects. Only people have artwork (`people.image_url`); Jellyfin's studio logos (Thumb) come from an artwork provider blockbustr doesn't have, and that is the contract's one ignore.
  - **`enableTotalRecordCount=false` gives `TotalRecordCount: 0`** here (observed), not the page length as on `/Items`.
  - `isFavorite=true` returns an empty result: names can't be favourites yet.
- **`/Artists`** returns an empty QueryResult (no music). Jellyfin Android's search always asks for it.
- **`/Items/Filters`** (legacy: genre names, ratings, years) and **`/Items/Filters2`** (genre `{Name, Id}`, `AudioLanguages`/`SubtitleLanguages` as `{Name: "English (eng)", Value: "eng"}` sorted by label) are aggregated in one query (`pg.ItemFilters`) over the same scope. Filters2 may set `recursive=false`. `Tags` is always `[]` until tags are stored. All captures match Jellyfin's **values**, genre ids included.
- **`/Search/Hints`** (no captured client uses it): library items ranked as in `/Items` search, plus remote matches (§7.4), then genres, studios and people. `includeItemTypes`/`excludeItemTypes` and `includeMedia|Genres|Studios|People=false` choose the kinds. Paging is applied over the combined list, and the total is the sum. Hints carry their own or the series' thumb/backdrop and `Artists: []`.

### 3.10 Valid empties and display preferences (implemented, P1.23: `handlers/misc.go`)
- **Empty QueryResult** (`{Items:[], TotalRecordCount:0, StartIndex:0}`): `/Items/{id}/Collections`, `/Items/{id}/Intros` (+ legacy `/Users/{u}/Items/{id}/Intros`), `/MediaSegments/{id}`, `/LiveTv/Programs/Recommended`.
- **Bare `[]`:** SpecialFeatures and LocalTrailers (+ legacy), `/SyncPlay/List`, `/web/ConfigurationPages`, `/Localization/Cultures|Countries|ParentalRatings|Options`; admin only: `/Plugins`, `/Packages`, `/ScheduledTasks`.
- **ThemeMedia:** theme songs and videos owned by the requested item, soundtracks by the zero id, all empty. `/Items/{id}/ThemeSongs|ThemeVideos` return one empty `ThemeMediaResult`.
- **Unknown routes** get a bare 404 with no body, like Kestrel, so plugin probes such as `/Streamyfin/config` read as "not installed".
- **`/DisplayPreferences/{key}`** (GET/POST, `?client=`, default `emby`) is stored per (user, key, client) in `display_preferences.data`. GET returns 12.1.0's defaults for a key nobody saved (SortName/Ascending, 250×250, Horizontal, ShowBackdrop) with `CustomPrefs: {}`. POST fills unset fields with those defaults and answers 204. **`Id` is the MD5 of the key's UTF-16LE bytes as a .NET Guid, dashed** (observed: `usersettings` → `3ce5b65d-e116-d731-65d1-efc4a30ec35c`). Prefs saved from the jellyfin-web capture read back value-identical.

### 3.11 User data writes (implemented, P1.24: `handlers/userdata.go`, `queries/userdata.sql`)
- **Routes:** `POST`/`DELETE /UserPlayedItems/{id}` and `/UserFavoriteItems/{id}`, the legacy `/Users/{u}/PlayedItems/{id}` and `/Users/{u}/FavoriteItems/{id}` (not in 12.1.0's spec, but Jellyfin Android uses the favourite one), and `GET`/`POST /UserItems/{id}/UserData`.
- **Response:** every route answers **200 with the item's updated `UserItemDataDto`**, built by the listing builder, so a Series or Season includes `UnplayedItemCount`/`PlayedPercentage`. The item must be visible (§3.7), otherwise the answer is 404. A non-admin naming another user gets 403.
- **Played** applies to every Movie/Episode at or below the item (an episode, a season's or series' episodes, or a whole library):
  - Played = true and the resume point is cleared.
  - PlayCount becomes at least 1, and **only increments when `datePlayed` is given**, so re-ticking "watched" isn't a new play.
  - The last played date is `datePlayed`, else the stored date, else now.
- **Unplayed** resets the same set: not played, PlayCount 0, no resume point, no last played date. A series reset this way drops out of NextUp, because it no longer counts as started (§3.8).
- **Favourite** applies to the item itself; favouriting a series doesn't touch its episodes.
- **`POST /UserItems/{id}/UserData`** stores only the fields the body sets (Played, PlayCount, PlaybackPositionTicks, IsFavorite, Rating, LastPlayedDate) on the item itself. `Likes`, `Key` and the derived counts are ignored.
- **Not yet:**
  - `UserDataChanged` WebSocket events (P2.10).
  - `/UserItems/{id}/Rating` (likes).
  - Jellyfin's user-data `Key` for library items is a provider id (a movie's TMDB id, e.g. `"931285"`); blockbustr sends the dashed item id. Only the shape is checked, and no captured client reads it.

## 4. Data model (PostgreSQL)

Extensions: `pgcrypto`, `pg_trgm`, `unaccent`.

```sql
libraries(id uuid pk, name, kind text check in ('movies','tvshows','mixed','stremio'),
          paths text[], options jsonb, created_at)

items(id uuid pk, library_id fk, parent_id uuid null fk items, top_parent_id uuid,
      type text  -- Movie|Series|Season|Episode|Folder|CollectionFolder|BoxSet
      name, original_title, sort_name, forced_sort_name,
      index_number int, parent_index_number int,          -- episode / season numbers
      production_year int, premiere_date date, end_date date,
      overview text, tagline text, official_rating text,
      community_rating real, critic_rating real, runtime_ticks bigint,
      provider_ids jsonb,          -- {"Tmdb":"603","Imdb":"tt0133093","Tvdb":"…"}
      source_kind text check in ('file','strm','stremio','virtual'),
      path text, strm_url text, stremio_ref jsonb,         -- {addonId,type,id}
      etag text, date_created timestamptz, date_modified timestamptz,
      date_last_refreshed timestamptz, is_missing bool default false,
      search tsvector generated always as (to_tsvector('simple', unaccent(name)||' '||coalesce(original_title,''))) stored)
  idx: (parent_id, sort_name), (library_id, type, date_created desc),
       (top_parent_id, type), gin(search), gin(name gin_trgm_ops), gin(provider_ids jsonb_path_ops),
       unique(library_id, path) where path is not null

media_sources(id uuid pk, item_id fk, name, container, size bigint, bitrate int,
              path_or_url text, protocol text check in ('File','Http'),
              is_remote bool, probed_at timestamptz, probe_error text)
media_streams(media_source_id fk, idx int, type text  -- Video|Audio|Subtitle
              codec, language, title, is_default bool, is_forced bool, is_external bool,
              width int, height int, bitrate int, channels int, channel_layout text,
              video_range text, profile text, level real, pixel_format text, delivery_url text,
              pk(media_source_id, idx))
chapters(item_id, idx, start_ticks, name)

genres(id pk, name unique) ; item_genres(item_id, genre_id)
studios(id pk, name unique) ; item_studios(item_id, studio_id)
people(id uuid pk, name, provider_ids jsonb) ; item_people(item_id, person_id, kind, role, sort_order)
images(item_id, type text, idx int, source_url text, local_path text, tag text, blurhash text,
       width int, height int, pk(item_id, type, idx))

users(id uuid pk, name unique citext, password_hash, is_admin bool, is_disabled bool,
      policy jsonb, configuration jsonb, created_at, last_login_at)
devices(id text pk  -- client DeviceId
        user_id fk, name, app_name, app_version, last_seen_at, capabilities jsonb)
access_tokens(token_sha bytea pk, user_id fk, device_id fk, created_at, last_used_at, revoked_at)
user_data(user_id, item_id, played bool, play_count int, playback_position_ticks bigint,
          is_favorite bool, rating real, last_played_at timestamptz, audio_stream_idx int,
          subtitle_stream_idx int, updated_at, pk(user_id, item_id))
  idx: (user_id, last_played_at desc) where playback_position_ticks > 0   -- Resume
display_preferences(user_id, pref_id, client, data jsonb, pk(user_id, pref_id, client))

stremio_addons(id uuid pk, manifest_url unique, manifest jsonb, config jsonb,
               enabled bool, priority int, last_fetched_at)
stremio_catalogs(addon_id fk, catalog_type, catalog_id, library_id fk null, enabled bool,
                 pk(addon_id, catalog_type, catalog_id))

debrid_accounts(id uuid pk, provider text, api_key_enc bytea, enabled bool, priority int)
library_events(id bigserial pk, item_id, kind text, at timestamptz)  -- powers Kodi sync + LibraryChanged
```

**Implemented (P1.3)** in `internal/store/pg/migrations/0000{1..4}_*.sql`, embedded in the binary and applied automatically on every start (`blockbustr -migrate up|down|down-all|status` for manual control). Differences from the sketch above:
- Enumerations (`items.type`, `source_kind`, `media_streams.type`, `images.type`, `libraries.kind`) are `text` + `CHECK`, not Postgres enums, so adding a value is a one-line migration.
- Full-text search is an **expression index** on the IMMUTABLE `item_search_vector(name, original_title)` (which uses `f_unaccent()`, because `unaccent()` itself is only STABLE), not a stored `search` column. That keeps `SELECT *` mapped onto sqlc's `db.Item`. Queries must filter with `item_search_vector(name, original_title) @@ …` to use the index. The trigram index is on `f_unaccent(name)`.
- Added `items.index_number_end` (multi-episode files) and CHECKs (`strm` needs `strm_url`, `stremio` needs `stremio_ref`). `images.tag` is NOT NULL. `access_tokens.token_sha` must be 32 bytes.
- `genres`, `studios`, `people`, `chapters`, `stremio_*`, `debrid_accounts` and `library_events` come with the tasks that use them (P1.16, P3.x, P4.2). `items.original_language` (TMDB ISO 639-1) was added by migration 00010, which also re-opens matched movies/series for one metadata refresh so the language is filled in.

Queries live in `internal/store/pg/queries/*.sql` and are compiled with sqlc (`sqlc.yaml`, pgx/v5) into `internal/store/pg/db`. Types: `uuid.UUID`, `json.RawMessage` (jsonb, nil = NULL), `time.Time`, pointers for nullable columns. Request handling uses a `pgxpool.Pool` (`pg.NewPool`); migrations use `database/sql`. **Never edit `internal/store/pg/db` by hand.** Run `make generate`; `make sqlc-check` (part of `make check`) fails on drift. `/Items` filtering is built with a small query builder (squirrel-style, hand-rolled) over sqlc, because the filter space is too large for static queries.

## 5. Redis keys

| Key | Value | TTL |
|---|---|---|
| `tok:{sha256}` | `{userId,deviceId}` | 24h sliding |
| `sess:{deviceId}` | session JSON (now playing, position, capabilities) | 10 min, refreshed by ping/progress |
| `sessions` | set of active deviceIds | — |
| `link:{sourceHash}` | resolved CDN URL | provider-specific, default 4h (RD links ~ 1 day) |
| `probe:{sourceHash}` | ffprobe JSON (also persisted in PG) | 30 d |
| `stremio:streams:{addon}:{type}:{id}` | addon stream list | 30 min |
| `stremio:meta:{addon}:{type}:{id}` | addon meta | 24h |
| `q:latest:{userId}:{parentId}` / `q:nextup:{userId}` | cached query result | 5 min, purged on events |
| `play:{playSessionId}` | PlaybackInfo decisions per media source + chosen tracks, user, device (P2.2) | 24h |
| `lock:transcode:{playSessionId}` | owner | 30s, renewed |
| `lock:scan:{libraryId}` | owner | 10 min, renewed |
| `rl:{provider}` | token bucket | — |
| `qc:{secret}` | QuickConnect state | 10 min |
| `search:{source}:{kind}:{term}` | remote search results for one source (normalised lower-case term) | 1h |
| channel `events` | pub/sub of typed events | — |

Redis is a **cache and coordination layer only**. Everything has to be rebuildable from Postgres, and a cold Redis must never lose user data.

**Implemented (P1.5)** in `internal/cache`, on go-redis v9:
- Every key is prefixed `bb:`, so a Redis can be shared with other apps.
- Key builders and TTL constants live in `keys.go`. Never build key strings anywhere else.
- `SetJSON`, `GetJSON`, and `GetJSONEx` (sliding TTL, for tokens) all treat a miss as `ok=false`, not an error.
- `TryLock` / `Unlock` / `Refresh` is a single-instance lock: `SET NX PX` plus a random owner token, with owner-checked Lua for unlock and refresh. It's meant for scans and transcode sessions.
- Startup fails fast if Redis is unreachable.

## 6. Library and scanning

1. A scan is triggered by startup, fsnotify, a schedule (default 6h), `POST /Library/Refresh`, or a Stremio catalog sync.
2. Walk the paths and classify each file by extension: video, `.strm`, `.nfo`, images, subtitles (`.srt/.ass/.vtt` sidecars).
3. Parse names with the ported jellybird parser into Movie (Title, Year) or Episode (Series, Season, Episode[, EpisodeEnd]). Specials go to Season 0, and extras folders (`Featurettes`, `Extras`, `Behind The Scenes`…) are excluded from Movies.
4. Upsert the hierarchy `CollectionFolder → Series → Season → Episode` or `CollectionFolder → Movie`. A Series is keyed by folder, a Season by `(series, number)`.
5. Local files go through ffprobe in a bounded worker pool (default `NumCPU/2`). **`.strm` files are not probed during a scan.**
6. Metadata: local `.nfo` wins, otherwise TMDB lookup (title + year → tmdb id → details, credits, images). Images are stored as `source_url` and fetched lazily on first request.
7. Diff the file set against the DB. Missing files are marked `is_missing`, then deleted after a grace period (default 24h, so a flaky mount doesn't wipe the library).
8. Emit `LibraryChanged` with added/updated/removed ids, coalesced over 2s windows.

### Images (implemented, P1.17: `internal/images`, `handlers/images.go`)
- `GET`/`HEAD /Items/{id}/Images/{type}[/{index}]` is **public**, as in Jellyfin. Type names are case-insensitive. People are addressed by the same URL (`/Items/{personId}/Images/Primary` → `people.image_url`).
- The original is fetched once (TMDB URL or local file; http(s), must decode as an image, ≤30 MB) to `paths.images/orig/xx/<tag>`. Each requested size is rendered once to `paths.images/r/xx/<tag>/<w>x<h>-q<q>.jpg`.
- Sizing: `width/height` and `maxWidth/maxHeight` fit inside the box; `fillWidth/fillHeight` cover it (the client crops). Images are never upscaled, and results round up to 20 px to bound the cache. Concurrent identical requests share one fetch/render (singleflight); files are written atomically.
- **Headers like 12.1.0:** `Cache-Control: public, max-age=31536000, immutable` when the URL has `tag=`, otherwise `public`; `ETag: "<tag>"`; `Last-Modified`; `304` on revalidation. A missing image returns 404 with a JSON string: `"Luca does not have an image of type Thumb"`.
- **Format:** JPEG, or PNG for PNG sources (logos). Jellyfin answers WebP to browsers that accept it, but there's no pure-Go WebP encoder and every captured client accepts JPEG, so `Vary: Accept` is not sent.
- A post-scan `images` hook computes dimensions and **blurhash** (3×4/4×3 components from a 32 px thumbnail) for each library's images, so `ImageBlurHashes` can be sent before an image is ever requested.
- **Not yet:** cache eviction, prefetching person images, local artwork (`poster.jpg`, `fanart.jpg`).

### Metadata (implemented, P1.16: `internal/metadata`)
- Runs after every successful library scan for items that were never refreshed, are unmatched and older than 7 days, or are matched and older than 30 days. Series run before Seasons before Episodes.
- **Local `.nfo` first:** `<stem>.nfo` or `movie.nfo` for movies, `tvshow.nfo` in the series folder, `<stem>.nfo` for episodes. A URL-only nfo contributes its IMDb/TMDB id.
- **TMDB:** the id comes from the nfo, from `/find` by IMDb id, or from a search by title + year. A match needs an exact title (case, accents and punctuation ignored) within ±1 year, or a single result with a fitting year. **A wrong match is worse than none.** Seasons and episodes use the parent series' TMDB id, and each season is fetched once per run.
- **Merge rule:** the nfo wins wherever it has a value; TMDB fills the rest (`metadata_source` = `nfo`, `tmdb` or `nfo+tmdb`).
- **Stored:** names (sort names re-derived for movies/series; episode/season sort names stay number-based), overview, tagline, certification for the metadata language's country, community rating, premiere/end dates, year, provider ids, genres, studios (TMDB networks for shows, like Jellyfin), people (top-25 cast, directors, writers, episode guest stars; deterministic person ids so people are shared across titles), and Primary/Backdrop/Logo URLs. Image tags are a hash of the URL.
- TMDB season names that only repeat the show name are ignored ("Season 1" stays).
- **Rescans never overwrite** a name/sort name/year once metadata is applied.
- Without `BLOCKBUSTR_TMDB_API_KEY`, only `.nfo` files are used.

### Probing (implemented, P1.13: `internal/media`)
- `Prober.Probe(ctx, target, Options{Remote})` runs `ffprobe -show_format -show_streams -show_chapters`. Remote targets add `-probesize 10M -analyzeduration 5M`, and the caller's context deadline (15 s for `.strm`) kills a stuck probe.
- `media.Info` keeps **all** streams (Jellyfin lists image subtitles too), chapters, and an attachment count (MKV fonts, needed for ASS burn-in later).
- Per-stream bitrate falls back to Matroska's `BPS` tag. `VideoRange`/`VideoRangeType` use Jellyfin's enum names: SDR, HDR10, HLG, DOVI, DOVIWithHDR10/HLG/SDR/EL. **HDR10+ is not detected** (it needs frame-level side data), so DV + HDR10+ reports `DOVIWithHDR10`.

### Scanner (implemented, P1.14: `internal/library`)
- Libraries come from `config.yaml` `libraries:` and are synced by name at startup, each with one CollectionFolder item.
- Scans run on start, every `scan.interval` (6h), and on `POST /Library/Refresh`, one library at a time under the Redis scan lock (refreshed during long scans).
- **Movies:** one item per file; title and year from the file name, falling back to the folder (`folderTitle`, which keeps "The Office (US)" intact).
- **TV:** top-level folder = Series. A `Season NN`/`S03`/`Specials` folder or the parsed SxxEyy gives the Season. The episode title is the text after `SxxEyy… - `, up to the release tags.
- **Files:** a file etag is `size-mtime`, and a `.strm` etag is a hash of the target. Only items whose source etag differs are (re)probed, in a pool of `scan.probe_workers`. Probe failures are stored on the source and not retried until the file changes.
- **Missing files:** unseen items get `missing_since` (hidden) and are deleted after `scan.missing_grace` (24h); empty Seasons/Series are removed afterwards.
- Seen-ness is compared on the **database clock** (`DBNow`), not the app's.
- **Watching (P1.14b, `library/watch.go`):** with `scan.watch` (default on), every folder under each library path is watched with fsnotify (inotify isn't recursive, so new folders are added as they appear). A change rescans **that library only** (`TriggerLibrary`), once nothing under it has changed for `scan.watch_delay` (30s), so a long copy is one scan. Ignored: hidden files and folders, and partial downloads (`.part`, `.tmp`, `.crdownload`, `.!qB`…). A lost-events overflow rescans every library. A missing root, or hitting `fs.inotify.max_user_watches`, is logged; those folders rely on `scan.interval`, as do network shares, which send no events. Changes on the host reach the container through the bind mount (verified live).
- **Not yet:** multi-file movie versions grouped into one item, sidecar subtitles (P2.8).

### `.strm`
- **Observed in Jellyfin 12.1.0 (recon, Findroid 1.1.0):** for a `.strm`, `PlaybackInfo` returns a MediaSource with `Protocol: "Http"`, `IsRemote: true`, `Path` = **the raw `.strm` URL**, and `SupportsDirectPlay/DirectStream/Transcoding` all `false`, even though the streams were probed. The client then opens `Path` itself and never calls `/Videos/{id}/stream`. So playback only works if the *client device* can reach the `.strm` host, and the URL (including any `sig=`/token) is exposed to every client.
- **blockbustr does it differently:** `Path` and `DirectStreamUrl` always point at blockbustr's own `/Videos/{id}/stream?static=true&MediaSourceId=…`, with `Protocol: "File"`-style semantics. That endpoint 302s to the resolved URL (or proxies it, §8.2). Clients only need to reach blockbustr, and raw URLs are never sent. Contract tests must allow this deliberate difference.
- Content is a single URL (trimmed; `#` comments ignored).
- If the URL host matches a jellybird `/stream/…` or blockbustr resolver URL, it is mapped internally to a `resolve.Source` and the HTTP hop is skipped.
- On first PlaybackInfo, resolve and ffprobe (`-analyzeduration 5M -probesize 10M`, 15s timeout), then persist the streams. If probing fails, return a MediaSource with minimal info (container guessed from the URL, `SupportsDirectPlay: true`) so playback can still be tried.
- **Implemented (P2.4b):**
  - `Scanner.ProbeRemote` (`library/remote.go`) runs on the first PlaybackInfo of a source never probed. One probe runs per item (singleflight), and the result replaces the item's source. A failure is recorded (`probe_error`) and not retried until the `.strm` changes.
  - **Media sources never carry the target.** A remote source's `Path` is `{server}/Videos/{id}/stream?static=true&MediaSourceId={msId}` (`Protocol: Http`, so clients open it), built from `server.external_url` or the request's scheme and host. This is a deliberate difference from Jellyfin, which exposes the URL.

## 7. Stremio addons

### 7.1 Protocol client
- `GET {base}/manifest.json` → id, name, version, `resources` (catalog, meta, stream, subtitles), `types`, `catalogs[]`, `idPrefixes`.
- `GET {base}/catalog/{type}/{id}[/{extra}].json` → `{metas:[…]}`. Extras: `skip=N`, `search=…`, `genre=…`.
- `GET {base}/meta/{type}/{id}.json` → `{meta:{…, videos:[{id, season, episode, title, released}]}}`.
- `GET {base}/stream/{type}/{id}.json` → `{streams:[{url | infoHash+fileIdx | externalUrl, name, title, behaviorHints:{bingeGroup, filename, videoSize}}]}`. Series ids look like `tt1234567:1:2`.
- `GET {base}/subtitles/{type}/{id}.json` → `{subtitles:[{url, lang}]}`.
- Configured addons (Torrentio, Comet, …) embed their config in the base URL. Treat the whole URL as a secret.

### 7.2 Catalogs as libraries
- Each enabled `stremio_catalogs` row maps to a `stremio` library (or a BoxSet inside one).
- A sync job pages the catalog (default 2 pages / 100 items, configurable) and upserts items with `source_kind='stremio'` and `stremio_ref`. Metadata is enriched from TMDB when the id is IMDb.
- Series episodes are created from `meta.videos`.
- Search across addons is the core feature described in §7.4.

### 7.3 Stream resolution (at PlaybackInfo)
1. Collect streams from every enabled addon that lists `stream` for the type and id prefix, in parallel, with a 6s timeout per addon. Cache the results (§5).
2. **Rank** with a score: resolution (2160 > 1080 > 720), whether it's cached on debrid (a big bonus; addons often mark `[RD+]`, or we check with the provider), codec preference vs the DeviceProfile (HEVC/AV1 penalised if the client can't decode them), size within the user's bitrate cap, the release-group allow/deny list, and the language.
3. Take the top N (default 3) → one `MediaSource` each, with `Name` = a short label (`2160p HDR • 18.2 GB • RD+`).
4. Resolve when the stream is requested, not at PlaybackInfo: `url` is used directly. `infoHash` goes through `provider.AddMagnet` → select file (`fileIdx`, or the largest video) → unrestrict → CDN URL, cached in `link:*`.
5. Not cached on any debrid: return the source with `Name` suffixed `• not cached` and start the debrid download in the background. Never block PlaybackInfo for more than ~8s.

### 7.4 In-client search and discovery (core feature)
Every captured client searches through `GET /Items?searchTerm=…&recursive=true` (Jellyfin Android sends three in parallel, split by `includeItemTypes`/`excludeItemTypes`/`mediaTypes`; §3.5). blockbustr answers those with **library matches first, then remote matches**, so the app's normal search becomes "search everything".

1. **Sources** (pluggable `search.Provider`, queried in parallel with a ~1.5s budget, results cached in Redis `search:*`):
   - **Cinemeta** (`https://v3-cinemeta.strem.io`, Stremio's official metadata addon): `/catalog/{movie|series}/top/search={term}.json`. No API key; this is the default.
   - **TMDB** `/search/multi` when `metadata.tmdb_api_key` is set (better artwork, localisation).
   - Every enabled **Stremio addon with a search-capable catalog** (`extra: [{name: "search"}]` in its manifest).
2. **Merging:**
   - Dedupe by provider IDs (IMDb > TMDB > TVDB). If a title is already in the library, show only the library item.
   - Map the request's `includeItemTypes`/`excludeItemTypes`/`mediaTypes` to movie/series and respect them. Remote items never match `LiveTvProgram`, `Audio`, etc.
   - Respect `limit` per source.
3. **Stable, addressable items.** Clients immediately call `/Items/{id}`, `/Items/{id}/Images/Primary`, `PlaybackInfo`, favourites, and so on with the IDs they got.
   - Each remote result is **upserted into Postgres** as an item in a hidden built-in `discover` library (`source_kind='stremio'`, `stremio_ref` = `{type, id: "tt0133093"}`, provider IDs, poster/backdrop URLs in `images`).
   - The item ID is **deterministic**: UUIDv5(namespace, `"imdb:tt0133093"`, or `"tmdb:movie:603"` if there's no IMDb ID). A repeated search returns the same ID, and a client's cached ID keeps working.
   - The `discover` library is not listed in `/UserViews`.
   - GC: `discover` items nobody has played, favourited or resumed are deleted after `search.retention` (default 7 days). Playing or favouriting one keeps it.
4. **Series on demand.** When a remote Series is opened (`/Shows/{id}/Seasons` or `/Episodes`), fetch its episode list from the addon's `meta` (`videos[]`) or TMDB, and upsert Seasons/Episodes with deterministic IDs (`"imdb:tt0944947:1:2"`).
5. **Playback:** `PlaybackInfo` on a remote item runs the §7.3 stream collection and ranking, then debrid resolution. Nothing has to be "added" first. Optionally (config), playing a title also adds it to the debrid cloud so it's cached next time.
6. **Client presentation, to verify with real apps (risk R6):**
   - Remote items carry `LocationType: "FileSystem"`, because some apps hide `Virtual`.
   - They carry a **placeholder MediaSource** (`Protocol: Http`, no streams yet) because some apps hide the Play button when `MediaSources` is empty.
   - The `Overview` gets a short source line ("Not in your library · via Torrentio").
   - Verify per client in the P3 manual matrix.
7. **Later (optional):** `/Items/Suggestions` and the home rows can also be filled from addon catalogs ("Trending on Cinemeta"), driven by the same machinery.

## 8. Playback

### 8.1 PlaybackInfo
Input: `DeviceProfile`, `MaxStreamingBitrate`, `StartTimeTicks`, `AudioStreamIndex`, `SubtitleStreamIndex`, `MediaSourceId`, `EnableDirectPlay/DirectStream/Transcoding`, `AutoOpenLiveStream`.

For each MediaSource, run `media.Decide(profile, source)` (ported from jollyrogarr's `decide.go`, extended to read Jellyfin's `DirectPlayProfiles`, `TranscodingProfiles`, `CodecProfiles` and `SubtitleProfiles`):
- **DirectPlay**: container, video codec and audio codec are all supported, and the bitrate is under the cap → `SupportsDirectPlay=true`, `Path`/`DirectStreamUrl` = `/Videos/{id}/stream?static=true&MediaSourceId=…&api_key=…`.
- **DirectStream (remux)**: the codecs are OK but the container isn't (e.g. MKV → client wants MP4/TS) → HLS with `-c copy`.
- **Transcode**: otherwise → `TranscodingUrl=/Videos/{id}/master.m3u8?…`, `TranscodingSubProtocol=hls`, `TranscodingContainer=ts|mp4`.

The response carries a `PlaySessionId` (uuid). Remember the chosen decision in Redis under that id.

**Decision engine (implemented, P2.1: `internal/media/decide.go`).** `media.Decide(profile, source, options)` has its own `DeviceProfile` types mirroring Jellyfin's JSON (media doesn't import jfapi), so the request's profile decodes straight into them.
- **Direct play** needs all of:
  - a Video DirectPlayProfile that takes the container and both codecs (empty list = any);
  - Container and CodecProfile conditions that hold;
  - a source bitrate within `MaxStreamingBitrate` (the request's, else the profile's);
  - a chosen subtitle that doesn't need burning in.
- **Direct stream** = direct play, or *only* the container is wrong and the video can be copied into the first Video TranscodingProfile (remux). **Transcoding** = the profile has a Video TranscodingProfile. Each flag can be disabled per request.
- **Reasons** are Jellyfin TranscodeReason names, **sorted in its enum order**:
  - the direct play failures;
  - plus `VideoCodecNotSupported`/`AudioCodecNotSupported` when the transcoding profile can't take the source codec;
  - a failed condition maps to its property's reason (VideoLevel → VideoLevelNotSupported…);
  - an unknown property value fails a condition only when `IsRequired`.
- **Container** is the alias the client listed, in ffprobe order: a stored `mp4` is `mov` to Streamyfin and `mp4` to Jellyfin Android, as observed. Codec aliases: hevc/h265, srt/subrip, pgs/pgssub/hdmv_pgs_subtitle, vtt/webvtt, dts/dca, and `pcm` covers `pcm_*`.
- **Transcode target:**
  - the profile's codecs filtered to what we encode (video h264/hevc; audio aac/mp3/ac3/eac3/opus/flac/vorbis; observed, Jellyfin drops dts, mp1/mp2 and truehd);
  - `MaxAudioChannels`, with audio copied at its own bitrate when the profile takes it, else 192/384 kbps;
  - **video bitrate = limit − audio bitrate** (observed 6,000,000 − 160,461 = 5,839,539), never above the source's;
  - `MaxFramerate` = the source's frame rate.
- **Subtitles:** every subtitle stream gets a delivery method. **Embed** when playing or remuxing the file and the profile embeds that format. Otherwise **External** for text formats the profile takes as files, then **Hls** (text, HLS transcode), else **Encode** (burn-in). Observed: Findroid gets External SRT/ASS and Encode PGS; Streamyfin gets Embed for everything.
- **Verified against all 25 captured PlaybackInfo exchanges** (Findroid, Jellyfin Android, Streamyfin): the same DirectPlay/DirectStream/Transcoding flags, container name, every subtitle's delivery method (85 streams), and for the two transcodes the same reasons, codecs, bitrates, frame rate, channel limit and segment container as Jellyfin's TranscodingUrl. The one difference is intended: a remote `.strm` over the limit (Luca at 4 Mbps) transcodes, where Jellyfin ignores the limit (§8.3a, P2.4).

**PlaybackInfo endpoint (implemented, P2.2: `handlers/playback.go`).**
- **Routes:** `GET`/`POST /Items/{id}/PlaybackInfo`.
- **Request:** the PlaybackInfoDto body (camelCase accepted), with the query string filling anything the body leaves out. GET has no profile, so nothing is supported, as for Findroid's empty profile. Findroid builds its own `/Videos/{id}/stream` URL either way.
- **Response:** `{MediaSources, PlaySessionId}`, with a new 32-hex `PlaySessionId` per call. Each source is the §3.7 MediaSourceInfo with the §8.1 decision applied:
  - the Supports\* flags and the client's container name;
  - for a transcode or remux, `TranscodingUrl`, `TranscodingSubProtocol` and `TranscodingContainer`;
  - per subtitle, `DeliveryMethod`, plus for External a `DeliveryUrl` of the form `/Videos/{dashed id}/{msId}/Subtitles/{idx}/0/Stream.{srt|ass|ssa|vtt}?ApiKey={token}`.
- **Not in the response:** a direct-play stream URL. Jellyfin sends none either; clients build `/Videos/{id}/stream`.
- **TranscodingUrl:** `/videos/{dashed id}/master.m3u8?` followed by Jellyfin's parameter names in Jellyfin's order: DeviceId, MediaSourceId, VideoCodec, AudioCodec, AudioStreamIndex, [SubtitleStreamIndex], VideoBitrate, AudioBitrate, MaxFramerate, SegmentContainer, PlaySessionId, ApiKey, [TranscodingMaxAudioChannels], RequireAvc, EnableAudioVbrEncoding, Tag (= source ETag), SubtitleMethod, [AllowVideoStreamCopy for a remux], TranscodeReasons.
  - Jellyfin's per-codec hints (`av1-level=…`, `hevc-rangetype=…`) are left out; our HLS endpoint (P2.6) is the only reader.
  - The `ApiKey` is the caller's token, as in Jellyfin, because players fetch playlists without headers.
- **Filtering and errors:** `mediaSourceId` narrows the sources. A non-playable item (series, season) or a hidden one is 404, a malformed body 400, and another user's `userId` 403 unless admin.
- **Unprobed sources:** a `.strm` without streams isn't decided; it keeps the item-details answer until P2.4b probes it on first PlaybackInfo.
- **Play session:** the decisions are stored in Redis as `play:{playSessionId}` (24h, `cache.PlaySessionKey`), together with the user, device, item, chosen audio/subtitle tracks and start time. P2.3/P2.6 read it.
- **Contract:** all 25 captured requests replay with Jellyfin's shape and the same flags, container, transcoding protocol and container, subtitle delivery methods and URLs, and every TranscodingUrl parameter apart from session/device and the codec hints. The one exception is the intended remote-over-limit divergence (§8.3a).

### 8.2 Stream endpoint
`/Videos/{id}/stream?static=true`:
- Local file: `http.ServeContent` (Range support). **Implemented (P2.3: `handlers/stream.go`).**
  - Routes: `GET`/`HEAD /Videos/{id}/stream[.{container}]`.
  - **Public, like Jellyfin 12.1.0:** Findroid sends no token at all. The item only has to be visible (enabled library, not missing) and playable.
  - The source is picked by `mediaSourceId` (default: the first).
  - **Headers as captured:** `Content-Length`, `Accept-Ranges: bytes`, `Last-Modified` (file mtime), `Content-Range` on 206, and no ETag. `bytes=0-` answers 206 with the whole file, and `If-Modified-Since` gives 304.
  - **`Content-Type`** follows the container the client names (`container=` or the route extension: Streamyfin's `container=mov` → `video/quicktime`), else the probed container, else the extension.
  - All 14 captured stream requests (Findroid, Jellyfin Android, Streamyfin) replay with Jellyfin's status and headers, against sparse stand-ins of the recorded size and mtime.
  - Not static yet: `static=false` still serves the file. Progressive transcodes go through HLS (P2.6).
- Remote source: **implemented (P2.4b: `internal/resolve`, `remoteStream` in `handlers/stream.go`).**
  - `resolve.Resolver` follows the `.strm` target's redirects with a one-byte range GET and keeps the final URL and size in Redis `link:{sha256(target)}` for 4h. A play doesn't re-run jellybird's unrestrict.
  - **Redirect or proxy:** the client is redirected (302) when `compat.redirect_clients` names it, or when the link keeps the request's scheme (TLS or `X-Forwarded-Proto: https`). Otherwise the link is **proxied**: GET/HEAD with `Range`/`If-Range` passed on, and `Content-Length`/`Content-Range`/`Accept-Ranges`/`Last-Modified` passed back. `compat.proxy_clients` always proxies.
  - A 401/403/404/410 from the CDN drops the cached link and resolves once more. An unresolvable source is 502.
  - Content-Type: as for local files, plus the resolved URL's extension for an unprobed `.strm`.
  - Verified live: Luca `.strm` → jellybird → https CDN, proxied over http as 206 `video/x-matroska`, and the bytes are Matroska.
- Remote (strm/debrid/stremio): **302 to the resolved URL** if the redirect is safe for this client, otherwise **reverse-proxy** with Range passthrough.
  - **Observed (recon, 2026-10-05):** Findroid 1.1.0 with **ExoPlayer** silently fails on `http://` → 302 → `https://` CDN. Its position stays at 0 and it reports "paused", because ExoPlayer refuses cross-protocol redirects by default. The same item plays with Findroid's **mpv** player. Most Android clients (Jellyfin Android, Android TV, Findroid default) use ExoPlayer/Media3.
  - **Rule:** only redirect when the scheme is kept (https → https, or http → http). When blockbustr is reached over plain `http` and the target is `https`, proxy by default. Override per client with `compat.redirect_clients` / `compat.proxy_clients` (matched on the `Client` auth field).
  - Recommended deployment: put blockbustr behind https (Tailscale `serve`, Caddy). Then redirects work for every client and the CDN bytes never pass through the server.

### 8.3 HLS (remux and transcode)
- `master.m3u8` gives one variant. `main.m3u8` is generated up front from the duration (segment length 3s, as Jellyfin 12.1.0), so seeking is possible before transcoding catches up.
- A segment request for `n` that's far from the current ffmpeg position restarts ffmpeg with `-ss` at `n × segment length` (a seek-aware session). That fixes jollyrogarr's "no seek during transcode" gap.
- Input is a local path or the resolved URL (ffmpeg reads http with `-reconnect 1 -reconnect_streamed 1`).
- Hwaccel comes from the ported `hwaccel.go` (VAAPI/QSV, NVENC later). Sessions are killed after 60s with no segment requests, or on `DELETE /Videos/ActiveEncodings?PlaySessionId=`.
- **Implemented (P2.5: `internal/transcode`).**
  - **Encoder:** `hwaccel.go` was ported unchanged. At startup each backend gets a real test encode and `transcode.hwaccel` picks one; the dev container logs `encoder=qsv available=[vaapi qsv software]`. The image needs `libmfx-gen1.2` (the oneVPL GPU runtime) for QSV; without it every QSV session fails with MFX error -9.
  - **`Manager`:** one ffmpeg per play session writes `{n}.ts` (MPEG-TS) into `paths.transcode/{playSessionId}/`. It enforces `transcode.max_sessions`, closes sessions idle for 60s, and clears stale directories at startup. `Start` waits for the first segment; `Restart(id, n)` relaunches from segment n with the same options and keeps segments already written.
  - **ffmpeg arguments:**
    - Input is a path or a URL (`-reconnect*` for remote).
    - Explicit `-map 0:v:0` plus the chosen audio track; subtitles never go in the TS.
    - Video is copied or encoded at the client's bitrate (`-b:v`/`-maxrate`, `-bufsize` 2×). Audio is copied or encoded with bitrate and channel limit.
    - The CPU tonemap chain is used for HDR, as in jollyrogarr.
    - `-force_key_frames` every segment, plus **`-forced_idr 1` on QSV** (`-forced-idr` on NVENC). Without it h264_qsv writes 10.7s segments, which was caught live; VAAPI and x264 honour forced keyframes as is.
    - `-ss`/`-output_ts_offset` and `-start_number` let a restart continue the timeline.
    - `hls_flags temp_file`, so a segment file only exists once complete.
  - **Verified:** real ffmpeg tests (remux, transcode with the chosen track and downmix, exact segment lengths, restart at segment 6 ≈ 12s, missing input) and a real-QSV test on this machine. Live in the container: Mortal Kombat II (AV1 1080p) → h264_qsv at 5.84 Mbps gives 6.0s segments at about 3.9× real time, and VAAPI is the same.
  - **Not yet:** hardware decode (Jellyfin reached 24.5× with VAAPI decode, so AV1/HEVC software decode is the bottleneck), hardware tonemapping, scaling to the client's max resolution, image-subtitle burn-in (P2.8), and the Redis `lock:transcode:*` (single instance for now).
- Subtitles: text subs are delivered as external `.vtt` via the Subtitles endpoint, and image subs (PGS) are burned in only when the client requires it.
- **Endpoints (implemented, P2.6: `handlers/hls.go`):** `GET`/`HEAD /Videos/{id}/master.m3u8`, `GET /Videos/{id}/main.m3u8`, `GET /Videos/{id}/hls1/{playlist}/{n}.ts` and `DELETE /Videos/ActiveEncodings`. All need a token; players use the URL's `ApiKey`.
  - **Stateless:** the transcoding URL carries everything (codecs, bitrates, `AudioStreamIndex`, `AllowVideoStreamCopy`, channel limit), so the ffmpeg options are derived from the query plus the item's stored streams, as in Jellyfin. A Redis flush or a server restart mid-play only costs a new ffmpeg start.
  - **master.m3u8:** one variant with `BANDWIDTH` = `AVERAGE-BANDWIDTH` = VideoBitrate + AudioBitrate, `VIDEO-RANGE`, `CODECS` (`avc1.640029` for our H.264 output, source codec when copied), `RESOLUTION` and `FRAME-RATE` of the source. It links to `main.m3u8?{same query}`, with `AudioCodec=copy` when the audio is copied, as Jellyfin does. Matches every captured master apart from CODECS and the scaled RESOLUTION (no scaling yet).
  - **main.m3u8:** the whole VOD playlist from the runtime, in Jellyfin's exact format: `#EXT-X-PLAYLIST-TYPE:VOD`, `VERSION:3`, `TARGETDURATION`, `#EXTINF:3.000000, nodesc`, `hls1/main/{n}.ts?{query}&runtimeTicks=…&actualSegmentLengthTicks=…`, `#EXT-X-ENDLIST`. **Streamyfin's captured request reproduces Jellyfin's 1,747,624-byte playlist byte for byte in size.** Segments are 3s, as in Jellyfin 12.1.0 (`transcode.segment_seconds`, default changed from 6).
  - **Segments:** ffmpeg starts on the first segment request, at that segment. An existing segment is served at once (`ServeContent`: 206 for `bytes=0-`, `video/mp2t`). One at most 4 segments ahead of ffmpeg is waited for (30s). Anything else, a seek forward or behind this run's start, restarts ffmpeg at that segment. A per-play-session lock serialises starts and restarts, and segments past the runtime are 404.
  - **ActiveEncodings:** stops the `playSessionId`'s session, else every session of `deviceId` (or of the caller's device). Answers 204.
  - **Not yet:** scaling down for low bitrates (Jellyfin gave Streamyfin 1280x536 at 4 Mbps), fMP4 segments, and `-re`-style throttling (ffmpeg runs ahead at full speed).

### 8.3a Observed client behaviour (recon)
- **Streamyfin 0.55.0** (libmpv): sends a DeviceProfile that allows everything, so Jellyfin answers `SupportsDirectPlay=true` even for `.strm` (`Protocol: Http`). The player then fetches the raw `.strm` URL itself and follows the http→https redirect (mpv). Local MKV played as `DirectStream` through `/Videos/{id}/stream` using Range requests (`bytes=0-`, a tail read near EOF for MKV cues, then mid-file).
- **Request bodies can be camelCase.** Streamyfin posts `PlaybackInfo` as `{"userId", "deviceProfile", "maxStreamingBitrate": 8000000, "audioStreamIndex": 2, "mediaSourceId", "isPlayback", "autoOpenLiveStream"}`. **JSON decoding must be case-insensitive on keys** (Go's `encoding/json` already is; keep it that way and never switch to a strict decoder for Jellyfin DTOs).
- **Jellyfin 12.1.0 ignores `maxStreamingBitrate` for remote (`.strm`) sources.** This was checked by replaying Streamyfin's own `PlaybackInfo` at a 4 Mbps cap: the local MF Ghost (38 Mbps) gets a `TranscodingUrl`, but Luca `.strm` (7.9 Mbps) still gets `SupportsDirectPlay: true` and no transcode. Remote items only transcode when the client can't decode the codec. **blockbustr enforces the cap for every source kind** (file, strm, debrid, stremio). That's one of the main reasons to transcode debrid content at all: a phone on cellular can't pull a 60 Mbps remux.
- **Bitrate cap vs source bitrate:** with an 8 Mbps cap, Luca (4K HEVC DV, probed at 7.9 Mbps total) is correctly direct-played. `media.Decide` compares the cap with `MediaSource.Bitrate`, so remote sources **need a probed bitrate**. If one is missing, assume it's over the cap (prefer a transcode over a stall). **Implemented (P2.4):** the cap applies to every source, and a remote source of unknown bitrate counts as `media.UnknownRemoteBitrate` = 80 Mbps. So "maximum" client settings (≥ 120 Mbps) still direct play, while a 10 Mbps phone transcodes.
- **Jellyfin bug to avoid:** for `.strm`, `MediaSource.Size` is **80**, the size of the `.strm` file itself. blockbustr reports the remote `Content-Length` (from the resolver's HEAD/Range probe), or leaves `Size` out.
- Streamyfin **polls `GET /Sessions` about every 2s** while open (74 calls in the session). That endpoint must be cheap, so serve it from Redis `sessions`/`sess:*` (§5).
- Streamyfin uses `/socket`: the server sends `ForceKeepAlive`, then both sides exchange `KeepAlive`; the server pushes `UserDataChanged` and `LibraryChanged`. Clients send `POST /Sessions/Capabilities/Full` once after login.

### 8.4 Progress
`/Sessions/Playing*` → update the `sess:*` session in Redis on every call, and write `user_data` on Start, every 30s, and on Stop:
- watched past 90% → `played=true`, position reset
- under 5% → position not saved

Publish `UserDataChanged`.

## 9. Testing strategy
1. **Fixtures (Phase 0):** a stock Jellyfin (same version as the reported `Version`) behind mitmproxy, driven by each client app. `scripts/capture` dumps request/response JSON into `testdata/jellyfin/{client}/{scenario}/NNN.json`, and `scripts/scrub-fixtures` strips tokens and IDs.
2. **Contract tests:** for each fixture, send the same request to blockbustr (seeded with an equivalent library) and compare the **shape**: key sets, value types, and the null/omit pattern. Values are ignored except for enums and an allowlist. Lives in `internal/jfapi/contract_test.go`.
3. **Unit tests:** parser, decision, ranking, auth header parsing, ID/tick/time helpers. Table-driven.
4. **Integration:** testcontainers (PG+Redis) for login → views → items → playbackinfo → stream (302 / range), using a generated Jellyfin SDK client where practical.
5. **Manual client matrix:** run each phase's checklist in TASKS.md on real apps.
6. **Perf:** synthetic 50k-item seed (`scripts/seed`), `vegeta` against `/Items` and `/Shows/NextUp`.

## 10. Configuration
Implemented in `internal/config`. Annotated example: `config.example.yaml`. Precedence is defaults < YAML < `BLOCKBUSTR_*` env vars (an empty env var doesn't override). Unknown YAML keys are an error, so typos fail fast, and `Validate` reports every problem at once.
```yaml
server:    { listen: ":8096", external_url: "", server_name: blockbustr, shutdown_timeout: 10s }
database:  { url: "postgres://blockbustr:…@postgres:5432/blockbustr?sslmode=disable" }   # BLOCKBUSTR_DATABASE_URL
redis:     { url: "redis://redis:6379/0" }                                                # BLOCKBUSTR_REDIS_URL
paths:     { cache: /cache }        # transcode/ and images/ default to subdirs of cache
compat:    { reported_version: "12.1.0", product_name: "Jellyfin Server", proxy_clients: [], redirect_clients: [] }
transcode: { hwaccel: auto, segment_seconds: 3, max_sessions: 4 }   # hwaccel: auto|none|qsv|vaapi
metadata:  { tmdb_api_key: "", language: en-US }                    # BLOCKBUSTR_TMDB_API_KEY
debrid:    { realdebrid_api_key: "", torbox_api_key: "" }           # BLOCKBUSTR_REALDEBRID_API_KEY / _TORBOX_API_KEY
log:       { level: info, format: text }                            # format: text|json
```
Port 8096 matches Jellyfin, so clients find it with the default port. Secrets belong in env (`.env.example`), not YAML.

## 11. Open questions and risks
- **Q1 Reported version. Resolved 2026-10-05:** we claim **Jellyfin 12.1.0** (latest stable, released 2026-09-15; image `jellyfin/jellyfin:12.1.20260915-010956`). Fixtures and `openapi.json` come from that version.
- **Q2 ProductName.** Do any clients reject a server whose `ProductName` isn't "Jellyfin Server"? Test in Phase 0. If none do, use "blockbustr".
- **Q3 Server discovery.** Should we answer UDP 7359 "who is JellyfinServer?" discovery? Probably yes in Phase 2; it's cheap.
- **Q4 Kodi.** The add-on's "native/add-on paths" mode wants file paths. `.strm` / remote may only work in add-on mode, so verify.
- **R1 Uncached debrid streams** make first play slow. Mitigate by ranking cached streams first and pre-warming on "Add to favourites".
- **R6 Remote search items in clients.** Apps may hide items without MediaSources or with `LocationType: Virtual`, and may cache search results. Mitigate with a placeholder MediaSource, `FileSystem` location type, and deterministic IDs persisted in Postgres (§7.4). Verify per client.
- **R5 Redirect compatibility.** ExoPlayer clients won't follow http→https redirects (confirmed). Proxying costs server bandwidth, so recommend https.
- **R2 Remote ffprobe latency.** Probe lazily, cache, and fall back to minimal MediaSource info.
- **R3 Legal/trademark.** Clean-room API only. Branding must not use Jellyfin marks. Users are responsible for the addons they configure.
- **R4 API drift.** New Jellyfin releases change DTOs. Pin the openapi version and regenerate deliberately.
