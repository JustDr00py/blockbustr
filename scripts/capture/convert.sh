#!/usr/bin/env bash
# Convert a recon capture into raw fixtures using the mitmproxy image (no host install needed).
#   scripts/capture/convert.sh deploy/recon/captures/findroid-1.1.0-full.mitm findroid-1.1.0 [scenario]
#   scripts/capture/convert.sh CAPTURE CLIENT SCENARIO --keep-client REGEX --keep-ua REGEX   # split a shared capture
#   scripts/capture/convert.sh --test        # run the converter's unit tests
set -euo pipefail

IMAGE="docker.io/mitmproxy/mitmproxy:12.2.3"   # keep in sync with deploy/recon/compose.yml
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENGINE="$(command -v podman || command -v docker || { echo "need podman or docker" >&2; exit 1; })"
run() { "$ENGINE" run --rm --security-opt label=disable -v "$ROOT:/work" -w /work --entrypoint python3 "$IMAGE" "$@"; }

if [[ "${1:-}" == "--test" ]]; then
  run -m unittest discover -s scripts/capture -p 'test_*.py' -v
  exit
fi

[[ $# -ge 2 ]] || { sed -n '2,4p' "$0"; exit 2; }
capture="$(realpath --relative-to="$ROOT" "$1")"
run scripts/capture/mitm2fixtures.py "$capture" --client "$2" --scenario "${3:-full}" --out testdata/jellyfin/_raw "${@:4}"
