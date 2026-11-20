#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."

case "$#:${1-}" in
    0:|1:--live) ;;
    *) printf 'Usage: %s [--live]\n' "$0" >&2; exit 2 ;;
esac

for script in scripts/*.sh; do
    bash -n "$script"
done
if command -v shellcheck >/dev/null; then
    shellcheck -x scripts/*.sh
fi
printf 'PASS shell checks\n'

# shellcheck source=scripts/go-bin.sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
python3 -m py_compile bridge/tts_worker.py
node --check bridge/web/client.js
(
    cd bridge
    "$GO_BIN" mod verify
    "$GO_BIN" vet ./...
    "$GO_BIN" test -race ./...
)

host_tests=$(mktemp -d)
trap 'rm -rf -- "$host_tests"' EXIT
host_cc=(gcc -std=c11 -Wall -Wextra -Werror -I firmware/main)
"${host_cc[@]}" firmware/tests/vad_test.c firmware/main/vad.c -o "$host_tests/vad"
"$host_tests/vad"
printf 'PASS host VAD tests\n'

"${host_cc[@]}" firmware/tests/frame_test.c firmware/main/frame.c -o "$host_tests/frame"
"$host_tests/frame"
"${host_cc[@]}" firmware/tests/gfx_test.c firmware/main/{frame,gfx}.c -o "$host_tests/gfx"
"$host_tests/gfx"

if [[ -n $(git ls-files -- ':(glob)**/sdkconfig.local' ':(glob)**/bridge.env') ]]; then
    printf 'sdkconfig.local and bridge.env must not be tracked\n' >&2
    exit 1
fi
for view in --cached --; do
    status=0
    # Split the literal so this script passes its own check.
    git grep -lF -e '/var/'home/ "$view" || status=$?
    if (( status != 1 )); then
        printf 'Tracked-file path check failed\n' >&2
        exit 1
    fi
done
printf 'PASS tracked-file hygiene\n'

if [[ ${1-} == --live ]]; then
    (cd bridge && "$GO_BIN" test -tags=live -v -count=1 ./...)
fi
