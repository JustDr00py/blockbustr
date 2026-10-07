# Deploying blockbustr

`docker-compose.yml` runs blockbustr with PostgreSQL 17 and Redis 8.

```bash
cp .env.example .env                      # repo root; set POSTGRES_PASSWORD and API keys
mkdir -p deploy/data/config deploy/data/cache
podman-compose -f deploy/docker-compose.yml -f deploy/docker-compose.podman.yml up -d --build   # rootless podman
# docker compose -f deploy/docker-compose.yml up -d --build                                       # docker
podman-compose -f deploy/docker-compose.yml ps              # all three should be healthy
curl -s localhost:8096/healthz
```

| Variable (in `.env`) | Default | Purpose |
|---|---|---|
| `POSTGRES_PASSWORD` | `blockbustr` | Database password (change it) |
| `BLOCKBUSTR_ADMIN_USERNAME` / `BLOCKBUSTR_ADMIN_PASSWORD` | — | Administrator created on first start. While set, the password is re-applied on every start, which is how you recover a lost password |
| `BLOCKBUSTR_PORT` | `8096` | Host port. The recon stack also uses 8096, so pick another to run both |
| `MEDIA_DIR` | `~/Videos` | Media library, mounted read-only at `/media` |
| `RENDER_GID` | `video` | Host `render` group GID, if `/dev/dri/renderD128` isn't world-accessible |
| `POSTGRES_PORT` / `REDIS_PORT` | `5432` / `6379` | Loopback-only ports for host tools (migrations, sqlc, `make test-integration`) |

- Redis has no persistence by design: it only holds caches and session state that can be rebuilt from Postgres (DESIGN §5).
- The image runs as non-root uid 1000, and the container healthcheck is `blockbustr -healthcheck`.
- Migrations run automatically on start. Run them by hand with `podman exec blockbustr_blockbustr_1 /app/blockbustr -config /config/config.yaml -migrate status` (or `up`, `down`, `down-all`).
- **podman-compose:** `up -d --build` does not replace a running container. After code changes run `podman-compose -f deploy/docker-compose.yml build blockbustr && podman-compose -f deploy/docker-compose.yml up -d --force-recreate blockbustr`.
- **Rootless podman needs `docker-compose.podman.yml`** (`userns_mode: keep-id`). Without it the container user can't write `deploy/data/cache` (`mkdir /cache/images: permission denied`). `PODMAN_USERNS=keep-id` in the environment is *not* passed through by podman-compose.

## Backups

Postgres holds everything that matters: libraries, items, users, tokens, watch state, addons and debrid accounts. Redis has no persistence and is rebuilt from Postgres (a test empties it and checks that clients see exactly the same answers: `TestRedisRebuildFromPostgres`). The cache directory (`deploy/data/cache`: images, subtitles, transcode segments) is downloaded or generated again on demand.

Back up three things:
1. **The database**, with `deploy/backup.sh`. It runs `pg_dump --format=custom` inside the Postgres container and keeps the newest `KEEP` dumps (default 14) in `BACKUP_DIR` (default `deploy/backups/`, gitignored).
2. **`.env`**, and above all `BLOCKBUSTR_SECRET_KEY`. Addon URLs and debrid API keys are sealed with it in the database, so a restored database without the same key can't use them; they have to be entered again. Keep a copy somewhere other than the backups.
3. **`deploy/data/config/config.yaml`**, if you edited it.

```bash
deploy/backup.sh                                  # → deploy/backups/blockbustr-20261007-095908.dump
BACKUP_DIR=/mnt/nas/blockbustr KEEP=30 deploy/backup.sh
```

**Nightly, as a systemd user timer** (rootless podman runs under your user):

```ini
# ~/.config/systemd/user/blockbustr-backup.service
[Unit]
Description=blockbustr database backup

[Service]
Type=oneshot
Environment=BACKUP_DIR=%h/blockbustr-backups
ExecStart=%h/Documents/blockbustr/deploy/backup.sh

# ~/.config/systemd/user/blockbustr-backup.timer
[Unit]
Description=Nightly blockbustr database backup

[Timer]
OnCalendar=*-*-* 03:30
Persistent=true

[Install]
WantedBy=timers.target
```

```bash
systemctl --user daemon-reload && systemctl --user enable --now blockbustr-backup.timer
loginctl enable-linger "$USER"     # run user timers while you're logged out
```

With docker, a root crontab line does the same: `30 3 * * * /path/to/blockbustr/deploy/backup.sh`.

**Restore** (stop blockbustr first, so nothing writes during the restore):

```bash
podman stop blockbustr_blockbustr_1
podman exec -i blockbustr_postgres_1 pg_restore -U blockbustr -d blockbustr --clean --if-exists --no-owner < deploy/backups/blockbustr-20261007-095908.dump
podman restart blockbustr_redis_1   # drop caches (tokens, query results) from before the restore
podman start blockbustr_blockbustr_1
```

Restart Redis after every restore. A cached access token or query result could otherwise outlive the restored database for up to a day. A dump from an older blockbustr restores fine: migrations run on start. To check a dump without touching the live database, restore it into a scratch database (`CREATE DATABASE blockbustr_check`) and compare `SELECT count(*) FROM items`.
