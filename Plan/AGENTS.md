# AGENTS.md — working rules for blockbustr

Read this first, then `DESIGN.md` (how it works) and `TASKS.md` (what to do next).

## Mission
blockbustr is a media server written in Go that **speaks the Jellyfin client API**. Existing Jellyfin apps (Findroid, Jellyfin Android / Android TV, Swiftfin, Streamyfin, Infuse, the Jellyfin for Kodi add-on) must connect, browse, play, seek and resume without being modified.
It differs from Jellyfin in four ways:
- Postgres + Redis instead of SQLite
- `.strm` and remote sources are first-class
- built-in debrid (Real-Debrid / TorBox)
- any **Stremio addon** can be used as a catalog and stream source

## Non-goals (for now)
- Serving `jellyfin-web` (that's Phase 5; the MVP covers native clients only).
- Live TV, music, books, SyncPlay, the Jellyfin plugin ABI.
- Copying any Jellyfin C# source. Jellyfin is GPLv2; we reimplement the **HTTP API clean-room**, from the OpenAPI spec and observed traffic.
- Using the "Jellyfin" name or logo in branding. We only say it is "compatible with Jellyfin clients".

## Working process
1. Pick **one** unchecked item from `TASKS.md` → *Now*. Don't start another until it's done.
2. If a task needs a design decision that `DESIGN.md` doesn't cover, update `DESIGN.md` first (small PR-sized edit), then write the code.
3. When the task is done, tick it in `TASKS.md`, move the next item into *Now*, and record any new follow-ups under the right phase.
4. Keep changes scoped to the task. No drive-by refactors and no speculative abstractions.
5. Anything you can't resolve goes in `TASKS.md` → *Blocked*, with the question spelled out.

## Definition of done
- `make check` passes (`go vet ./...`, `go test ./...`, `golangci-lint run` v2, config in `.golangci.yml`).
- New Jellyfin endpoints have a **contract test** that compares the response shape against a recorded fixture in `testdata/jellyfin/` (see DESIGN.md §9).
- Migrations are reversible (goose `-- +goose Down`).
- `TASKS.md` is updated.

## Stack (keep to it unless DESIGN.md is changed)
| Concern | Choice |
|---|---|
| Language | Go 1.26 |
| HTTP | `github.com/go-chi/chi/v5` |
| Postgres | `github.com/jackc/pgx/v5` (pgxpool) + `sqlc` v1.31 for queries (`make generate`; never hand-edit `internal/store/pg/db`), `pressly/goose` v3 for migrations (embedded) |
| Redis | `github.com/redis/go-redis/v9` |
| WebSocket | `github.com/coder/websocket` |
| Config | YAML file + env overrides (same pattern as jellybird) |
| Logging | `log/slog` (JSON in prod) |
| DTOs | `internal/jfapi/dto`, generated from the pinned 12.1.0 `openapi.json` by `scripts/gen-dto.py` → oapi-codegen v2.8 (`go tool`, models only). `make dto`; `make dto-check` is in `check`; never hand-edit `types.gen.go` |
| Media | system `ffmpeg` / `ffprobe` binaries |
| Tests | stdlib `testing`, `testcontainers-go` for Postgres/Redis |

## Code to port (copy, then adapt; don't import across repos)
| From | To | Notes |
|---|---|---|
| `~/Documents/jellybird/internal/strm/` | `internal/strm/` | **ported (P1.12):** `parse.go` + tests unchanged, `IsExtra` from `writer.go`; the jellybird Writer (debrid sync) is not ported. `.strm` reader is new |
| `~/Documents/jellybird/internal/provider/` (+ `realdebrid/`, `torbox/`) | `internal/provider/` | `Provider` interface, retry/429 handling |
| `~/Documents/jellybird/internal/stream/resolver.go` | `internal/resolve/` | swap SQLite link cache → Redis |
| `~/Documents/jellybird/internal/indexers/torrentio/` | reference only | Torrentio becomes "just another Stremio addon" |
| `~/Documents/jellybird/internal/metadata/tmdb/` | `internal/metadata/tmdb/` | **ported (P1.16)** unchanged + `details.go` (movie/show/season details, title+year search, `/find`), rate limiting and 429 retries in `get` |
| `~/Documents/jellybird/internal/ratelimit/` | `internal/ratelimit/` | **ported (P1.16)** unchanged; per process, move to Redis buckets later |
| `~/Documents/jollyrogarr/internal/media/` (`probe.go`, `decide.go`) | `internal/media/` | **probe ported + extended (P1.13)**: full stream model, Jellyfin video ranges. **`decide.go` ported (P2.1)** and rewritten around the Jellyfin DeviceProfile (its test cases kept); jollyrogarr's `IsHDR` not needed (`VideoRangeType` from the probe) |
| `~/Documents/jollyrogarr/internal/transcode/` (`session.go`, `hwaccel.go`) | `internal/transcode/` | **ported (P2.5):** `hwaccel.go` + tests unchanged; `session.go` adapted (remote input, explicit maps, copy/encode per stream, bitrate targets, segment-aligned IDR keyframes, numbered segments, idle reaper, session limit); jollyrogarr's real-ffmpeg tests rewritten for the new behaviour. Exposed through Jellyfin HLS URLs in P2.6 |
| `~/Documents/jollyrogarr/internal/auth/` | `internal/auth/` | password hashing only; token model is new |

Bring the matching `_test.go` files along and keep them passing.

## Jellyfin compatibility rules (don't break these)
- JSON casing: **PascalCase unless the client explicitly asks for `profile=CamelCase`** (DESIGN §3.1). Always respond through `jfapi.WriteJSON` with dto types and decode with `jfapi.DecodeJSON`. Never write JSON by hand, never build camelCase names yourself, and never rename dictionary keys.
- We report **Jellyfin 12.1.0** (DESIGN §11 Q1). Item, user and session IDs go over the wire as **32-char lowercase hex with no dashes** (`"N"` GUID format). Accept the dashed form on input too.
- Null and empty handling comes from the generated DTOs (`internal/jfapi/dto`). Optional fields are pointers with `omitempty`: nil means omitted, and a non-nil empty slice is sent as `[]`. The one field Jellyfin 12.1.0 always sends as `null` (`BaseItemDto.ChannelId`) is never omitted, configured in `scripts/gen-dto.py`. `TestFixturesRoundTrip` proves every captured response re-encodes identically, so add new endpoints to it.
- **Routes and query parameter names are case-insensitive.** Register routes on `jfapi.Router` (canonicalised automatically), read parameters with `jfapi.URLParam`, and read queries with `jfapi.QueryOf` (never `r.URL.Query()` directly). Never compare raw paths.
- Accept auth from every form: `Authorization: MediaBrowser …` and `?ApiKey=` (the only ones stock 12.1.0 accepts), plus the legacy `X-Emby-Authorization`, `X-Emby-Token`, `X-MediaBrowser-Token` and `?api_key=` while `compat.legacy_auth` is on (DESIGN §3.2).
- Times use `dto.Time`: UTC + `Z`, 100 ns precision. A non-zero fraction has trailing zeros trimmed (`…34.529331Z`); a zero fraction is written as `.0000000Z`, exactly like .NET. Durations are **ticks** (1 tick = 100 ns; `dto.TicksFromDuration`). IDs use `dto.ID` (N format out, any spelling in). Never hand-format either.
- Keep the legacy routes working (`/Users/{userId}/Items/...`) alongside the current ones (`/Items?userId=`).
- Unknown query params are ignored, never rejected.
- If a client sends something we don't support, return a **valid empty result**, not a 404/500 (clients often crash on errors).

## Repo layout
See DESIGN.md §2. Everything that serves Jellyfin's API lives under `internal/jfapi/`; domain logic (library, resolve, transcode) must not import `jfapi`.

## Safety
- Never commit `.env`, API keys, debrid tokens, or mitmproxy captures with real tokens. Scrub fixtures (`scripts/scrub-fixtures`).
- `.strm` URLs given to clients must be **signed, expiring blockbustr URLs**, never raw debrid links that carry account tokens.
