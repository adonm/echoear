#!/usr/bin/env bash
set -euo pipefail

scripts=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
# shellcheck source=scripts/board.sh
source "$scripts/board.sh"
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
    ECHOEAR_BRIDGE_TOKEN=$(python3 - "$env_file" <<'PY'
import shlex
import sys
from pathlib import Path

value = ""
for line in Path(sys.argv[1]).read_text().splitlines():
    name, sep, raw = line.partition("=")
    if sep and name.strip() == "ECHOEAR_BRIDGE_TOKEN":
        value = raw
try:
    parts = shlex.split(value)
except ValueError:
    raise SystemExit("Invalid bridge token assignment") from None
if len(parts) > 1:
    raise SystemExit("Invalid bridge token assignment")
print(parts[0] if parts else "")
PY
    )
fi
if [[ ! ${ECHOEAR_BRIDGE_TOKEN:-} =~ ^[[:graph:]]+$ ]]; then
    echo "Set ECHOEAR_BRIDGE_TOKEN or add it to $env_file." >&2
    exit 1
fi
base="http://$host"
checked_partition() {
    python3 -c '
import json, sys
status = json.load(sys.stdin)
board = status.get("board")
if board is None and status.get("device", "").startswith("echoear-"):
    board = "esp-vocat"  # Legacy vocat 0.8.0 did not report its board.
if board != sys.argv[1]:
    actual = board or "unknown board"
    raise SystemExit(f"OTA refused: expected {sys.argv[1]}, got {actual}")
print(status["running_partition"])
' "$board_name"
}
before=$(curl --noproxy '*' -fsS --max-time 5 "$base/status")
slot=$(checked_partition <<<"$before")
if (( build )); then
    "$scripts/build.sh"
fi
image="$scripts/../firmware/$build_dir/$board_image"
[[ -s "$image" ]] || { echo "Firmware image not found: $image" >&2; exit 1; }

# Pass the header via stdin so the token is not an argument in the process list.
printf 'X-EchoEar-Token: %s\n' "$ECHOEAR_BRIDGE_TOKEN" |
    curl --noproxy '*' -fsS --max-time 180 -H @- -H 'Expect:' -H 'Content-Type: application/octet-stream' \
        --data-binary "@$image" "$base/ota/upload"

for (( attempt=0; attempt<45; attempt++ )); do
    if status=$(curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 "$base/status" 2>/dev/null) &&
        current=$(checked_partition <<<"$status") &&
        [[ "$current" != "$slot" && "$current" =~ ^ota_[01]$ ]]; then
        printf '%s\n' "$status"
        exit 0
    fi
    sleep 1
done
echo "Device did not return on a different OTA partition: $base/status" >&2
exit 1
