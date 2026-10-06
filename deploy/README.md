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
