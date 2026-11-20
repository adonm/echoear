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

python3 -m py_compile bridge/{backend,server,state,test_bridge,tts}.py
python3 -O bridge/test_bridge.py --unit

vad_test=$(mktemp)
trap 'rm -f -- "$vad_test"' EXIT
gcc -std=c11 -Wall -Wextra -Werror -I firmware/main \
    firmware/tests/vad_test.c firmware/main/vad.c -o "$vad_test"
"$vad_test"
printf 'PASS host VAD tests\n'

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
    python3 bridge/test_bridge.py
fi
