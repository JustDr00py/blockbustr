#!/usr/bin/env bash
# Load test (TASKS P4.5, DESIGN §1 goal 2 and §9.6): starts blockbustr with
# scripts/loadtest/config.yaml against a database filled by scripts/seed,
# warms Redis, then sends each query the captured clients send with vegeta
# and reports its latency. The goal is p95 < 50 ms for every endpoint.
#
#   scripts/loadtest/run.sh                # seed if needed, run, report
#   RATE=100 DURATION=30s scripts/loadtest/run.sh
#
# Needs: go, curl, jq, vegeta (go install github.com/tsenart/vegeta/v12@v12.12.0)
# and the dev stack's Postgres and Redis on 127.0.0.1 (deploy/docker-compose.yml).
# The database blockbustr_perf must exist (CREATE DATABASE blockbustr_perf).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
RATE=${RATE:-50}            # requests per second, per endpoint
DURATION=${DURATION:-20s}   # per endpoint
GOAL_MS=${GOAL_MS:-50}      # p95 goal
DB_URL=${DB_URL:-postgres://blockbustr:blockbustr@127.0.0.1:5432/blockbustr_perf?sslmode=disable}
base=http://127.0.0.1:8199
out=${OUT:-$(mktemp -d /tmp/blockbustr-loadtest.XXXXXX)}
mkdir -p "$out"
vegeta=$(command -v vegeta || echo "$(go env GOPATH)/bin/vegeta")
[ -x "$vegeta" ] || { echo "vegeta not found: go install github.com/tsenart/vegeta/v12@v12.12.0" >&2; exit 1; }

echo "== seed (skipped when already seeded)"
(cd "$root" && go run ./scripts/seed -database-url "$DB_URL" 2>&1 | grep -v 'migration applied' || true) |
	sed 's/^/   /'

echo "== build and start the server"
(cd "$root" && go build -o "$out/blockbustr" ./cmd/blockbustr)
# A clean environment: BLOCKBUSTR_* variables would override the config.
env -i PATH="$PATH" HOME="$HOME" "$out/blockbustr" -config "$here/config.yaml" >"$out/server.log" 2>&1 &
server=$!
trap 'kill $server 2>/dev/null; wait $server 2>/dev/null' EXIT
for _ in $(seq 100); do
	curl -sf "$base/System/Info/Public" >/dev/null && break
	sleep 0.2
done
curl -sf "$base/System/Info/Public" >/dev/null || { cat "$out/server.log" >&2; exit 1; }

auth='MediaBrowser Client="loadtest", Device="loadtest", DeviceId="loadtest-1", Version="1.0"'
login=$(curl -sf -X POST "$base/Users/AuthenticateByName" -H "Authorization: $auth" \
	-H 'Content-Type: application/json' -d '{"Username":"perf","Pw":"perf"}')
token=$(jq -r .AccessToken <<<"$login")
user=$(jq -r .User.Id <<<"$login")
hdr="Authorization: $auth, Token=\"$token\""
get() { curl -sf -H "$hdr" "$base$1"; }

views=$(get "/UserViews?userId=$user")
movies=$(jq -r '.Items[] | select(.Name=="Perf Movies") | .Id' <<<"$views")
shows=$(jq -r '.Items[] | select(.Name=="Perf Shows") | .Id' <<<"$views")
series=$(get "/Items?parentId=$shows&includeItemTypes=Series&sortBy=SortName&limit=1&startIndex=40" | jq -r '.Items[0].Id')
season=$(get "/Shows/$series/Seasons?userId=$user" | jq -r '.Items[0].Id')
movie=$(get "/Items?parentId=$movies&includeItemTypes=Movie&sortBy=SortName&limit=1&startIndex=12345" | jq -r '.Items[0].Id')
genre=$(get "/Genres?parentId=$movies&limit=1&startIndex=6" | jq -r '.Items[0].Id')

img='enableImageTypes=Primary,Backdrop,Thumb&imageTypeLimit=1'
# name|path: the shapes Findroid, Streamyfin and Jellyfin Android send (testdata/jellyfin).
cat >"$out/targets.txt" <<EOF
items-movies-page1|/Items?userId=$user&parentId=$movies&includeItemTypes=Movie&recursive=true&sortBy=SortName&sortOrder=Ascending&fields=PrimaryImageAspectRatio,MediaSourceCount&$img&startIndex=0&limit=100
items-movies-deep|/Items?userId=$user&parentId=$movies&includeItemTypes=Movie&recursive=true&sortBy=SortName&sortOrder=Ascending&fields=PrimaryImageAspectRatio,MediaSourceCount&$img&startIndex=20000&limit=100
items-movies-newest|/Items?userId=$user&parentId=$movies&includeItemTypes=Movie&recursive=true&sortBy=DateCreated,SortName&sortOrder=Descending&fields=PrimaryImageAspectRatio&$img&limit=50
items-shows-page1|/Items?userId=$user&parentId=$shows&includeItemTypes=Series&recursive=true&sortBy=SortName&sortOrder=Ascending&fields=PrimaryImageAspectRatio&$img&startIndex=0&limit=100
items-genre|/Items?userId=$user&parentId=$movies&genreIds=$genre&includeItemTypes=Movie&recursive=true&sortBy=SortName&fields=PrimaryImageAspectRatio&$img&limit=100
items-favorites|/Items?userId=$user&filters=IsFavorite&includeItemTypes=Movie,Series&recursive=true&sortBy=SortName&fields=PrimaryImageAspectRatio&$img&limit=100
items-search|/Items?userId=$user&searchTerm=amber%20echo&includeItemTypes=Movie&recursive=true&isMissing=false&enableTotalRecordCount=false&fields=PrimaryImageAspectRatio&imageTypeLimit=1&limit=100
items-detail|/Users/$user/Items/$movie
items-latest|/Items/Latest?userId=$user&parentId=$movies&fields=PrimaryImageAspectRatio&enableImageTypes=Primary&imageTypeLimit=1&limit=16
items-resume|/UserItems/Resume?userId=$user&includeItemTypes=Movie&enableImageTypes=Primary&fields=Overview&imageTypeLimit=1&limit=12
episodes|/Shows/$series/Episodes?userId=$user&seasonId=$season&fields=Overview,PrimaryImageAspectRatio
nextup|/Shows/NextUp?userId=$user&enableResumable=false&enableRewatching=false&enableTotalRecordCount=false&fields=PrimaryImageAspectRatio&enableImageTypes=Primary&imageTypeLimit=1&limit=24
EOF

echo "== warm up (each target 20x)"
while IFS='|' read -r name path; do
	for _ in $(seq 20); do get "$path" >/dev/null || { echo "   $name failed: $path" >&2; exit 1; }; done
done <"$out/targets.txt"

echo "== attack: $RATE req/s for $DURATION per endpoint (goal p95 < ${GOAL_MS} ms)"
printf '   %-20s %9s %9s %9s %9s %5s  %s\n' endpoint p50 p95 p99 max ok% result
fail=0
while IFS='|' read -r name path; do
	printf 'GET %s%s\n%s\n\n' "$base" "$path" "$hdr" >"$out/$name.target"
	"$vegeta" attack -targets "$out/$name.target" -rate "$RATE" -duration "$DURATION" -timeout 5s >"$out/$name.bin"
	"$vegeta" report -type json <"$out/$name.bin" >"$out/$name.json"
	# Latencies are in ns; print ms to one decimal.
	read -r p50 p95 p99 max ok < <(jq -r '[(.latencies["50th"], .latencies["95th"], .latencies["99th"], .latencies.max)
		| . / 1e5 | round / 10] + [.success * 100 | round] | @tsv' "$out/$name.json")
	verdict=ok
	if awk "BEGIN{exit !($p95 >= $GOAL_MS)}" || [ "$ok" != 100 ]; then verdict=FAIL; fail=1; fi
	printf '   %-20s %7sms %7sms %7sms %7sms %5s  %s\n' "$name" "$p50" "$p95" "$p99" "$max" "$ok" "$verdict"
done <"$out/targets.txt"

echo "== results in $out (server.log, <endpoint>.json, <endpoint>.bin)"
exit $fail
