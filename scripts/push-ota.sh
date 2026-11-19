#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
build=1
host=
for arg in "$@"; do
    if [[ "$arg" == --no-build ]]; then
        build=0
    elif [[ -z "$host" && "$arg" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]]; then
        host=$arg
    else
        host=
        break
    fi
done
if [[ -z "$host" ]]; then
    echo "Usage: $0 [--no-build] <ip_or_host>" >&2
    exit 2
fi

env_file="${XDG_CONFIG_HOME:-$HOME/.config}/echoear/bridge.env"
if [[ -z ${ECHOEAR_BRIDGE_TOKEN:-} && -r "$env_file" ]]; then
    # shellcheck source=/dev/null
    source "$env_file"
fi
if [[ -z ${ECHOEAR_BRIDGE_TOKEN:-} ]]; then
    echo "Set ECHOEAR_BRIDGE_TOKEN or add it to $env_file." >&2
    exit 1
fi
if (( build )); then
    "$scripts/build.sh"
fi
image="$scripts/../firmware/build/echoear.bin"
[[ -s "$image" ]] || { echo "Firmware image not found: $image" >&2; exit 1; }
base="http://$host"
before=$(curl --noproxy '*' -fsS --max-time 5 "$base/status")
slot=$(python3 -c 'import json, sys; print(json.load(sys.stdin)["running_partition"])' <<<"$before")

# Pass the header via stdin so the token is not an argument in the process list.
printf 'X-EchoEar-Token: %s\n' "$ECHOEAR_BRIDGE_TOKEN" |
    curl --noproxy '*' -fsS --max-time 180 -H @- -H 'Expect:' -H 'Content-Type: application/octet-stream' \
        --data-binary "@$image" "$base/ota/upload"

for (( attempt=0; attempt<45; attempt++ )); do
    if status=$(curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 "$base/status" 2>/dev/null) &&
        current=$(python3 -c 'import json, sys; print(json.load(sys.stdin)["running_partition"])' <<<"$status") &&
        [[ "$current" != "$slot" && "$current" =~ ^ota_[01]$ ]]; then
        printf '%s\n' "$status"
        exit 0
    fi
    sleep 1
done
echo "Device did not return on a different OTA partition: $base/status" >&2
exit 1
