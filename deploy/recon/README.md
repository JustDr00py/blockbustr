# Recon stack (Phase 0)

Stock **Jellyfin 12.1.0** (`jellyfin/jellyfin:12.1.20260915-010956`) behind **mitmproxy 12.2.3** in reverse-proxy mode. Point real client apps at it and every request/response is recorded, so we can build blockbustr's contract fixtures (TASKS P0.3–P0.5).

| Port | What | Bound to |
|---|---|---|
| 8096 | mitmproxy → Jellyfin (**point clients here**) | all interfaces |
| 8081 | mitmweb UI (password `recon`) | localhost |
| 8097 | Jellyfin direct, no capture (admin/setup) | localhost |

## Usage
```bash
cd deploy/recon
mkdir -p data/jellyfin/{config,cache} data/mitmproxy captures
podman-compose -f compose.yml up -d        # docker compose works too
curl -s -H 'Accept: application/json' localhost:8096/System/Info/Public
```
1. Finish the setup wizard at <http://localhost:8097>, which bypasses the proxy so setup traffic stays out of the captures.
   - Create the user `recon`.
   - Add the libraries `/media/Movies` (Movies) and `/media/Series` (Shows).
2. Media comes from **`~/Videos`**, mounted read-only at `/media`. Override with `MEDIA_DIR=/path podman-compose … up -d`. SELinux labelling is disabled for the Jellyfin container only, so your files are never relabelled.
3. On each client, add the server manually as `http://<this-host-LAN-IP>:8096`. UDP discovery (7359) doesn't go through the proxy.
   - On Fedora you may need: `sudo firewall-cmd --add-port=8096/tcp`
4. Flows are appended to `captures/flows.mitm`. Watch them live at <http://localhost:8081>.
5. Between client sessions, rotate the file so each client's flows stay separate:
   ```bash
   podman-compose -f compose.yml stop mitmproxy
   mv captures/flows.mitm captures/<client>-<scenario>.mitm
   podman-compose -f compose.yml start mitmproxy
   ```

6. Turn a capture into raw fixtures (runs in the mitmproxy image, nothing to install):
   ```bash
   scripts/capture/convert.sh deploy/recon/captures/findroid-1.1.0-full.mitm findroid-1.1.0 [scenario]
   # → testdata/jellyfin/_raw/findroid-1.1.0/full/{NNNN-METHOD-endpoint.json, index.json}
   ```
7. Scrub them into committable fixtures (stdlib Python, runs on the host):
   ```bash
   python3 scripts/capture/scrub_fixtures.py findroid-1.1.0 [scenario]
   # → testdata/jellyfin/findroid-1.1.0/full/  (aborts and deletes the output if anything leaks)
   ```
   Put personal strings (hostnames, names) in `scripts/capture/scrub.local.txt`, one per line. That file is gitignored.

`data/` and `captures/` are gitignored. **Captures contain real access tokens.** Only scrubbed fixtures (P0.5) get committed.

## Findings so far
- `/System/Info/Public` → `Version: "12.1.0"`, `ProductName: "Jellyfin Server"`.
- **JSON casing (corrected in P1.7):** a configured server answers **PascalCase** for every Accept value *except* an explicit `profile=CamelCase` (first JSON range wins). Dictionary keys are never renamed. The camelCase seen at first came from the not-yet-set-up server.
- **Auth:** stock 12.1.0 has `EnableLegacyAuthorization=false`, so only `Authorization: MediaBrowser …` and `?ApiKey=` work; `X-Emby-*` and `?api_key=` get 401.
- `/api-docs/openapi.json` is served (about 1.9 MB). Use it for P1.6 DTO generation.
- **`.strm` in Jellyfin 12.1.0:** `PlaybackInfo` hands the client the raw `.strm` URL as `Path` (`Protocol: Http`, every `Supports*` flag false). The client fetches it directly and never touches `/Videos/{id}/stream`, so the *client device* must be able to reach the URL's host (e.g. `jellybird-host` over Tailscale). See DESIGN §6.
- **ExoPlayer vs redirects:** Findroid (ExoPlayer) can't play a `.strm` whose `http://` URL 302s to an `https://` CDN; it sits at position 0. With Findroid's mpv player it plays. See DESIGN §8.2.
- **QSV:** `/dev/dri` is passed through (Intel Arc 130V/140V, iHD 26.2.4). Hardware decode and encode for H.264, HEVC Main10 and AV1 are all available. Tested in the container: AV1→H.264 ran at 13× realtime, and 4K HEVC10 HDR (`.strm`)→1080p H.264 with `vpp_qsv` tone mapping ran at 6.8×.
