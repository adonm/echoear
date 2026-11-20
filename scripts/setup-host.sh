#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data_home=${XDG_DATA_HOME:-$HOME/.local/share}
data=$data_home/echoear
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

# shellcheck source=scripts/go-bin.sh
source "$REPO_DIR/scripts/go-bin.sh"
GO_BIN=$(resolve_go)
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
"$uv" pip install --python "$tts_python" 'kokoro-onnx==0.6.1'

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

bridge_bin="$data/echoear-bridge"
build_tmp=$(mktemp "$bridge_bin.XXXXXXXX")
trap 'rm -f -- "$build_tmp"' EXIT
(
    cd -- "$REPO_DIR/bridge"
    "$GO_BIN" build -trimpath -ldflags="-s -w" -o "$build_tmp" .
)
chmod 700 -- "$build_tmp"
mv -f -- "$build_tmp" "$bridge_bin"
trap - EXIT

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

hex_path=
if [[ -z $(systemctl --user show hex.service --property=FragmentPath --value) ]]; then
    hex_path=$(type -P hex)
fi
python3 - "$REPO_DIR" "$data_home" "$tts_python" "$bridge_bin" "$units" "$hex_path" <<'PY'
import re
import sys
from pathlib import Path

def escape(value):
    return value.translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "\n": "\\n", "\r": "\\r", "\t": "\\t",
    }))

repo, data, python, binary, directory, hex_path = sys.argv[1:]
units = Path(directory)
template = Path(repo, "bridge/echoear-bridge.service").read_text()
bridge = units / "echoear-bridge.service"
paths = {"REPO_DIR": repo, "DATA_HOME": data, "BRIDGE_BIN": binary, "TTS_PYTHON": python}
bridge.write_text(re.sub(r"@([A-Z_]+)@", lambda m: escape(paths[m[1]]), template))
bridge.chmod(0o644)

if hex_path:
    (units / "hex.service").write_text(f"""[Unit]
Description=Anomaly Hex

[Service]
Type=notify
ExecStart="{escape(hex_path)}" service
Restart=on-failure

[Install]
WantedBy=default.target
""")
PY
if "$pin_gpu"; then
    source="$REPO_DIR/config/systemd/10-vulkan-rx6600.conf"
    destination="$units/hex.service.d/10-vulkan-rx6600.conf"
    if ! cmp -s -- "$source" "$destination"; then
        install -m 644 -- "$source" "$destination"
    fi
fi

systemctl --user daemon-reload
systemctl --user enable hex.service echoear-bridge.service
systemctl --user restart hex.service
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
systemctl --user restart echoear-bridge.service
if [[ ! -e "$config/opencode/opencode.json" && ! -e "$config/opencode/opencode.jsonc" ]]; then
    ( set -o noclobber
      printf '{\n  "%s": "https://opencode.ai/config.json"\n}\n' "\$schema" > "$config/opencode/opencode.jsonc"
    )
fi
opencode mcp add echoear --global -- "$bridge_bin" mcp
opencode api get /api/info >/dev/null
printf 'Configure firmware/sdkconfig.local as shown in README.md. The bridge token is in %s.\n' "$env_file"
