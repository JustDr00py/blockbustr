#!/usr/bin/env bash
# Backs up blockbustr's database (TASKS P4.6, deploy/README.md "Backups").
# Postgres holds everything that matters; Redis and the cache directory are
# rebuilt from it (DESIGN §5). Writes a pg_dump custom-format file and keeps
# the newest KEEP of them.
#
#   deploy/backup.sh                         # → deploy/backups/blockbustr-<time>.dump
#   BACKUP_DIR=/mnt/nas/blockbustr KEEP=30 deploy/backup.sh
#
# Runs pg_dump inside the Postgres container, so the host needs no client.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
BACKUP_DIR=${BACKUP_DIR:-$here/backups}
KEEP=${KEEP:-14}
ENGINE=${ENGINE:-$(command -v podman >/dev/null && echo podman || echo docker)}
# podman-compose names it blockbustr_postgres_1, docker compose deploy-postgres-1.
PG_CONTAINER=${PG_CONTAINER:-$("$ENGINE" ps --format '{{.Names}}' | grep -m1 -E '(^|[_-])postgres([_-]1)?$' || true)}
[ -n "$PG_CONTAINER" ] || { echo "no running postgres container (set PG_CONTAINER)" >&2; exit 1; }

mkdir -p "$BACKUP_DIR"
out="$BACKUP_DIR/blockbustr-$(date +%Y%m%d-%H%M%S).dump"
# Write to a temporary name first, so a failed dump never looks like a backup.
"$ENGINE" exec "$PG_CONTAINER" pg_dump -U blockbustr -d blockbustr --format=custom >"$out.part"
mv "$out.part" "$out"
echo "wrote $out ($(du -h "$out" | cut -f1))"

# Keep the newest KEEP dumps.
ls -1t "$BACKUP_DIR"/blockbustr-*.dump | tail -n +$((KEEP + 1)) | xargs -r rm --
