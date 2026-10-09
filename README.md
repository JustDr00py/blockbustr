# blockbustr

A media server written in Go that **speaks the Jellyfin client API**, so the Jellyfin apps you already use (Findroid, Streamyfin, Jellyfin for Android / Android TV, Swiftfin, Infuse, Jellyfin for Kodi) connect to it unmodified. It is compatible with Jellyfin clients; it is not Jellyfin and is not affiliated with the Jellyfin project.

What it adds:

- **Remote media as first-class items.** `.strm` files, Real-Debrid / TorBox, and any **Stremio addon** (Torrentio, AIOStreams, Cinemeta, OpenSubtitles…) show up as normal movies and shows, with versions to pick from, audio and subtitle tracks, and resume.
- **Search finds anything.** The search box in any client returns your library plus titles from your addons that you don't have yet; picking one plays it through the addon's streams.
- **Built for large libraries.** PostgreSQL and Redis instead of SQLite. A 50,000-item library answers every list the apps ask for in under 50 ms at the 95th percentile (`make loadtest`).
- **Transcoding** with Intel Quick Sync / VAAPI or software ffmpeg, HLS for clients that need it, direct play and remux otherwise.
- **An admin web UI** at `/blockbustr/ui/` for libraries, users, addons, debrid accounts, server settings, logs and what's playing.

**Status: pre-1.0.** Daily use with Streamyfin, Findroid and Moonfin works: browsing, direct play, transcoding, seeking, subtitles, audio switching and resume across devices. The other clients are still being checked (`Plan/TASKS.md`). There is no web client yet: browsers and apps built on `jellyfin-web` can't connect.

## What it is, and what it isn't

blockbustr **is** a server for movies and shows, for people who stream from debrid services and Stremio addons alongside, or instead of, files they own, and who want to keep using the Jellyfin apps on their phones and TVs. It picks the version each device can play (resolution, codec, HDR and Dolby Vision, bitrate, language, cached on debrid), keeps debrid links on the server, and logs who played what.

It **isn't**:

- **Jellyfin.** It's a separate server that speaks the same client API, not a fork or a plugin. Jellyfin server plugins don't run on it.
- **A web player.** There's an admin UI, but no in-browser client; you watch in the Jellyfin apps.
- **A music, photo, book or live TV server.** Movies and shows only.
- **A downloader.** It doesn't fetch files into your library (that's what Sonarr/Radarr do), and it doesn't run a BitTorrent client: a torrent from an addon is played only through a debrid account.
- **A content source.** It finds nothing by itself. What it can play comes from your files, your debrid accounts and the addons you add, and what you stream is your responsibility.

## How it compares

These projects overlap with blockbustr. The table is from each project's README and docs as of October 2026, and they all move fast, so check them for anything that matters to you.

| | blockbustr | [Remux](https://github.com/lostb1t/remux) | [Silo](https://github.com/Silo-Server/silo-server) | Jellyfin + [Gelato](https://github.com/faggiolina/gelato) |
|---|---|---|---|---|
| What it is | Standalone server, Jellyfin API | Standalone server, Jellyfin API | Standalone server, Jellyfin/Emby API and its own apps | Jellyfin server with a plugin |
| Written in | Go | Rust | Go, React web UI | C# (.NET) |
| Stremio addons | Any, several at once | Any, several at once | No (proposed in [#287](https://github.com/Silo-Server/silo-server/issues/287)) | AIOStreams only |
| Debrid | Real-Debrid and TorBox accounts built in: cache checks, one torrent per hash, links never reach the apps | Through addons | No | Through AIOStreams |
| Torrents | Only through a debrid account | Built-in torrent engine | No | Through AIOStreams |
| Other sources | Local files, `.strm` | Local files, WebDAV, IPTV | Local files | Local files, `.strm` |
| Choosing a version | Ranked per device: resolution, codec, HDR/DV, bitrate, language, cached; per-user resolution cap | Not documented | n/a | AIOStreams' own ordering |
| Transcoding | Quick Sync, VAAPI or software | Not documented (ElfHosted's build: direct play only) | VAAPI, Quick Sync, NVENC | Jellyfin's (all hardware types) |
| Web client | No | Not documented | Yes | Yes (jellyfin-web) |
| Music and more | No | Music | Audiobooks, ebooks, podcasts, manga (beta) | Jellyfin's music, photos, live TV |
| Libraries | Folders, plus addon catalogs | Rule-based (tags, catalogs, popularity, year) | Folders | Folders, plus catalogs as libraries |
| Storage | PostgreSQL + Redis | Not documented | PostgreSQL + Redis | SQLite |
| License | MIT or Apache-2.0 | AGPL-3.0 | AGPL-3.0 | GPL-2.0 (Jellyfin) |

Some other options:

- **Stremio itself, with a debrid service:** the simplest setup if you watch on one or two devices and don't need your own library, accounts or the Jellyfin apps.
- **AIOStreams' Jellyfin mode** and **JellyStreams** (ElfHosted, closed source) serve addon results to Jellyfin apps without a server of your own. Neither transcodes or serves your own files.
- **[Stremfin](https://github.com/hfip/Stremfin)** and **[Polyfin](https://github.com/moodiness/polyfin)** are smaller bridges of the same kind, both early work in progress.
- **Plain Jellyfin, Emby or Plex** with Sonarr/Radarr if you'd rather download and own the files than stream them.

Choose something else when you need a web player, music or books (Silo, Jellyfin), WebDAV or rule-based libraries (Remux), Jellyfin's plugins (Jellyfin + Gelato), or NVIDIA hardware transcoding (Silo, Jellyfin).

## Quick start

Requires Docker or Podman with compose. The compose file builds from source; tagged releases also publish `linux/amd64` and `linux/arm64` images to `ghcr.io/justdr00py/blockbustr`.

```bash
git clone https://github.com/JustDr00py/blockbustr && cd blockbustr/deploy
cp .env.example .env              # admin user, BLOCKBUSTR_SECRET_KEY, MEDIA_DIR, API keys
mkdir -p data/config data/cache
cp ../config.example.yaml data/config/config.yaml   # set your libraries
docker compose up -d --build
```

Compose reads `.env` from `deploy/`, so run it there. Rootless Podman also needs `-f docker-compose.podman.yml`. See [`deploy/README.md`](deploy/README.md) for every variable, hardware transcoding, upgrades and [backups](deploy/README.md#backups).

Then add a server in your Jellyfin app at `http://<host>:8096` and sign in with the administrator from `deploy/.env`. The admin UI is at `http://<host>:8096/blockbustr/ui/`.

## Configuration

`config.yaml` (annotated in [`config.example.yaml`](config.example.yaml)) holds libraries, transcoding, metadata, addon and search settings. `BLOCKBUSTR_*` environment variables override it.

- **Libraries** are folders of movies or shows. Folders of `.strm` files work too.
- **Stremio addons and debrid accounts** are added in the admin UI. Their URLs and keys are stored encrypted with `BLOCKBUSTR_SECRET_KEY` (generate one with `openssl rand -hex 32`), so keep a copy of that key with your backups.
- **Metadata** comes from TMDB when `BLOCKBUSTR_TMDB_API_KEY` is set (or the key is entered on the admin UI's Settings page), otherwise from `.nfo` files and the addons.
- **Settings page.** Stream proxy hosts, the TMDB key, how many versions are offered and probed, preferred languages and release groups, transcoding limits and catalog sync can be changed in the admin UI and apply at once. A setting that `config.yaml` or the environment sets is locked there.

### Debrid traffic and bandwidth

Debrid links are never handed to the apps: blockbustr fetches a debrid or addon stream itself and passes it on, so the debrid service sees the server's address. From a VPS or a datacenter, that usually counts as the service's paid *remote traffic*; at home it uses your connection's download, and its upload for anyone watching from outside.

- **Stream proxy hosts.** When an addon's links go through a stream proxy (MediaFlow Proxy, StremThru, or the built-in proxy of a hosted AIOStreams), list its host on the Settings page. Apps then fetch from the proxy themselves and the bytes skip blockbustr. The debrid service sees the proxy's address instead.
- **Fewer, smaller versions.** *4K versions offered* set to 0 keeps 4K remuxes (often 20–60 GB a film) out of reach entirely. A user's *Highest addon resolution* (Users page) caps one user only: they're neither offered nor able to stream or download versions above it.
- **Probing.** Before a version is played, blockbustr reads about 10 MB of it to learn its audio and subtitle tracks. *Versions probed ahead* limits this to the best few versions per title; a version someone picks is probed when it's picked. Results are cached in Redis for 30 days.
- **Downloads** fetch the whole file at once. Turn off *Can download* for users who don't need it.
- **Transcoding** pulls the full source through the server even when the stream comes from a proxy.

## Building and developing

Go 1.26, Node 24 (admin UI), and Postgres 17 + Redis 8 for the integration tests (the compose stack's loopback ports work).

```bash
make build        # bin/blockbustr, version from `git describe`
make ui           # admin UI into internal/adminui/dist (embedded in the binary)
make check        # vet, generated code up to date, unit + integration tests, golangci-lint
make loadtest     # 50k-item seed + vegeta (Plan/DESIGN.md §9.6)
```

`Plan/DESIGN.md` explains how it works, `Plan/TASKS.md` what is done and next, and `Plan/AGENTS.md` the working rules.

**Releases** are tagged `vMAJOR.MINOR.PATCH` (semantic versioning; `v0.x` while the API layer is still filling in). Pushing a tag builds the multi-arch image and publishes it as `ghcr.io/justdr00py/blockbustr:<version>` and `:latest`. The binary reports its tag (`blockbustr -version`); to the apps it reports Jellyfin `12.1.0`, the API version it implements.

## Clean-room and trademarks

The Jellyfin HTTP API is reimplemented from its published OpenAPI description and from recorded traffic between the apps and a stock server. No Jellyfin source code is used. "Jellyfin" is used only to say which apps blockbustr works with; the project uses neither its name nor its logo as branding.

## License

Licensed under either of

- Apache License, Version 2.0 ([LICENSE-APACHE](LICENSE-APACHE))
- MIT license ([LICENSE-MIT](LICENSE-MIT))

at your option.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this work, as defined in the Apache-2.0 license, shall be dual licensed as above, without any additional terms or conditions.
