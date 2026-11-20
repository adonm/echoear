#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data=${XDG_DATA_HOME:-$HOME/.local/share}/echoear
units=$config/systemd/user
agents=$config/opencode/agents
env_file=$config/echoear/bridge.env
pin_gpu=false
case "$*" in
    "") ;;
    "--gpu-index 0") pin_gpu=true ;;
    -h|--help)
        printf '%s\n' 'Usage: setup-host.sh [--gpu-index 0]' \
            'Use --gpu-index 0 only if hex model devices lists your intended Vulkan GPU at index 0.'
        exit 0 ;;
    *) printf '%s\n' 'Usage: setup-host.sh [--gpu-index 0]' >&2; exit 2 ;;
esac

for cmd in curl hex opencode python3 sha256sum systemctl; do
    command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
done
systemctl --user show-environment >/dev/null

uv=${UV_BIN:-uv}
if [[ ! ${UV_BIN:-} ]]; then
    # The mise shim may not be active in a noninteractive shell.
    for candidate in "$HOME"/.local/share/mise/installs/uv/latest/uv-*/uv; do
        if [[ -x "$candidate" ]]; then
            uv=$candidate
            break
        fi
    done
fi
command -v "$uv" >/dev/null || { echo 'Install uv or set UV_BIN before running setup.' >&2; exit 1; }

tts_python="$data/tts-venv/bin/python"
mkdir -p -- "$data/tts-models"
if [[ ! -x "$tts_python" ]]; then
    "$uv" venv --python 3.13 "$data/tts-venv"
fi
"$uv" pip install --python "$tts_python" 'kokoro-onnx==0.6.1' 'scipy>=1.15,<2'

download_tts_model() {
    local name=$1 checksum=$2 destination="$data/tts-models/$1" temporary
    if [[ -f "$destination" ]] && printf '%s  %s\n' "$checksum" "$destination" | sha256sum --check --status; then
        return
    fi
    temporary=$(mktemp "$destination.XXXXXXXX")
    if curl --fail --silent --show-error --location \
        "https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.1/$name" \
        --output "$temporary" && printf '%s  %s\n' "$checksum" "$temporary" | sha256sum --check --status; then
        mv -f -- "$temporary" "$destination"
    else
        rm -f -- "$temporary"
        printf 'Failed to download and verify %s\n' "$name" >&2
        return 1
    fi
}
download_tts_model kokoro-v1.0.onnx beb0d1848dee9a49da392cc3df26958d46cfa35d321edf434f52949153f0df3a
download_tts_model voices-v1.0.bin bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d

mkdir -p -- "$agents" "$config/echoear" "$units/hex.service.d"
agent=$agents/echoear.md
if [[ ! "$agent" -ef "$REPO_DIR/agents/echoear.md" ]]; then
    if [[ -e "$agent" || -L "$agent" ]]; then
        printf 'Move the existing %s aside before running setup.\n' "$agent" >&2
        exit 1
    fi
    ln -s -- "$REPO_DIR/agents/echoear.md" "$agent"
fi

if [[ ! -e "$env_file" ]]; then
    token=$(python3 -c 'import secrets; print(secrets.token_hex(32))')
    ( set -o noclobber
      printf 'ECHOEAR_BRIDGE_HOST=0.0.0.0\nECHOEAR_BRIDGE_PORT=8765\nECHOEAR_BRIDGE_TOKEN=%s\n' "$token" > "$env_file"
    )
fi
chmod 600 -- "$env_file"

python3 - "$REPO_DIR" "$tts_python" "$units/echoear-bridge.service" <<'PY'
import sys
from pathlib import Path

def escape(value):
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%").replace("$", "$$")

repo, python, destination = sys.argv[1:]
template = Path(repo, "bridge/echoear-bridge.service").read_text()
unit = Path(destination)
unit.write_text(template.replace("@REPO_DIR@", escape(repo)).replace("@TTS_PYTHON@", escape(python)))
unit.chmod(0o644)
PY

# Reuse the installed Hex unit, or provision a minimal one for binary-only installs.
if [[ -z $(systemctl --user show hex.service --property=FragmentPath --value) ]]; then
    hex_path=$(type -P hex)
    cat > "$units/hex.service" <<EOF
[Unit]
Description=Anomaly Hex

[Service]
Type=notify
ExecStart=$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1].replace("%", "%%").replace("$", "$$"), ensure_ascii=False))' "$hex_path") service
Restart=on-failure

[Install]
WantedBy=default.target
EOF
fi
hex_changed=false
if "$pin_gpu"; then
    source="$REPO_DIR/config/systemd/10-vulkan-rx6600.conf"
    destination="$units/hex.service.d/10-vulkan-rx6600.conf"
    if ! cmp -s -- "$source" "$destination"; then
        install -m 644 -- "$source" "$destination"
        hex_changed=true
    fi
fi

systemctl --user daemon-reload
systemctl --user enable hex.service echoear-bridge.service
if "$hex_changed"; then
    systemctl --user restart hex.service
else
    systemctl --user start hex.service
fi
if [[ "$(hex settings get transcription_device)" != gpu ]]; then
    hex settings set transcription_device gpu
    systemctl --user restart hex.service
fi
model=parakeet_unified_en
if ! hex model status "$model" | grep -q '^installed[[:space:]]verified[[:space:]]'; then
    hex model install "$model"
fi
if [[ "$(hex settings get transcription.model)" != "$model" ]]; then
    hex model use "$model"
fi
opencode api get /api/info >/dev/null
systemctl --user restart echoear-bridge.service
printf '%s\n' 'Copy firmware/sdkconfig.local.example to firmware/sdkconfig.local.' \
    'Set Wi-Fi credentials and the LAN bridge URL in firmware/sdkconfig.local.'
printf 'Copy ECHOEAR_BRIDGE_TOKEN from %s to firmware/sdkconfig.local. Keep it private.\n' "$env_file"
