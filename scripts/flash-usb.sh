#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/uvx.sh
source "$scripts/uvx.sh"
if (( $# > 1 )); then
    echo "Usage: $0 [port]" >&2
    exit 2
fi
if (( $# )); then
    ports=("$1")
else
    shopt -s nullglob
    ports=(/dev/ttyACM*)
fi
if (( ! ${#ports[@]} )); then
    echo "No /dev/ttyACM* devices found." >&2
    exit 1
fi

build="$scripts/../firmware/build"
if [[ ! -s "$build/echoear.bin" || ! -s "$build/flash_args" ]]; then
    echo "Build the firmware with scripts/build.sh before flashing." >&2
    exit 1
fi
cd -- "$build"
failed=0
for port in "${ports[@]}"; do
    echo "Flashing $port"
    if ! run_uvx --from esptool esptool --chip esp32s3 --port "$port" --baud 460800 \
            write-flash --no-progress @flash_args; then
        failed=1
    fi
done
exit "$failed"
