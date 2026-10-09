# blockbustr

A media server written in Go that **speaks the Jellyfin client API**, so the Jellyfin apps you already use (Findroid, Streamyfin, Jellyfin for Android / Android TV, Swiftfin, Infuse, Jellyfin for Kodi) connect to it unmodified. It is compatible with Jellyfin clients; it is not Jellyfin and is not affiliated with the Jellyfin project.

What it adds:

- **Remote media as first-class items.** `.strm` files, Real-Debrid / TorBox, and any **Stremio addon** (Torrentio, AIOStreams, Cinemeta, OpenSubtitles…) show up as normal movies and shows, with versions to pick from, audio and subtitle tracks, and resume.
- **Search finds anything.** The search box in any client returns your library plus titles from your addons that you don't have yet; picking one plays it through the addon's streams.
- **Built for large libraries.** PostgreSQL and Redis instead of SQLite. A 50,000-item library answers every list the apps ask for in under 50 ms at the 95th percentile (`make loadtest`).
- **Transcoding** with Intel Quick Sync / VAAPI or software ffmpeg, HLS for clients that need it, direct play and remux otherwise.
- **An admin web UI** at `/blockbustr/ui/` for libraries, users, addons, debrid accounts, server settings, logs and what's playing.

**Status: pre-1.0.** Daily use with Streamyfin, Findroid and Moonfin works: browsing, direct play, transcoding, seeking, subtitles, audio switching and resume across devices. The other clients are still being checked (`Plan/TASKS.md`). There is no web client yet: browsers and apps built on `jellyfin-web` can't connect.

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
