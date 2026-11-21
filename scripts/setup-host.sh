#!/usr/bin/env bash
set -euo pipefail
umask 077

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
config=${XDG_CONFIG_HOME:-$HOME/.config}
data_home=${XDG_DATA_HOME:-$HOME/.local/share}
data=$data_home/echoear
units=$config/systemd/user
oc_root=$data/opencode
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
for cmd in curl gcc hex opencode python3 sha256sum systemctl tar; do
    command -v "$cmd" >/dev/null || { echo "Install $cmd before running setup." >&2; exit 1; }
done
systemctl --user show-environment >/dev/null

install_kokoro() (
    local name=kokoro-multi-lang-v1_0
    local checksum=60703707bc05ccd48d60e1eb723f38b07d6e474795a0b6a588d7220400e0f7a4
    local models="$data/tts-models" destination stage archive
    destination="$models/$name"
    if [[ -s "$destination/model.onnx" && -s "$destination/voices.bin" && -s "$destination/tokens.txt" &&
        -s "$destination/espeak-ng-data/phondata" ]]; then
        return 0
    fi
    if [[ -e "$destination" || -L "$destination" ]]; then
        printf 'Move the incomplete Kokoro model at %s aside before running setup.\n' "$destination" >&2
        return 1
    fi
    mkdir -p -- "$models"
    stage=$(mktemp -d "$models/.$name.XXXXXXXX")
    trap 'rm -rf -- "$stage"' EXIT
    archive="$stage/$name.tar.bz2"
    curl --fail --silent --show-error --location \
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/$name.tar.bz2" \
        --output "$archive"
    if ! printf '%s  %s\n' "$checksum" "$archive" | sha256sum --check --status; then
        printf 'Kokoro model archive failed SHA-256 verification.\n' >&2
        return 1
    fi
    tar -xjf "$archive" -C "$stage"
    mv -nT -- "$stage/$name" "$destination"
)
install_kokoro

bridge_bin="$data/echoear-bridge"
build_tmp=$(mktemp "$bridge_bin.XXXXXXXX")
trap 'rm -f -- "$build_tmp"' EXIT
(
    cd -- "$REPO_DIR/bridge"
    export CGO_ENABLED=1
    # Put the installed lib directory ahead of the dependency's cgo cache RPATH.
    export CGO_LDFLAGS="-Wl,-rpath,\$ORIGIN/lib${CGO_LDFLAGS:+ $CGO_LDFLAGS}"
    "$GO_BIN" build -trimpath -ldflags="-s -w -extldflags \"-Wl,-rpath,\$ORIGIN/lib\"" -o "$build_tmp" .
    case $("$GO_BIN" env GOARCH) in
        amd64) native=x86_64-unknown-linux-gnu ;;
        arm64) native=aarch64-unknown-linux-gnu ;;
        arm) native=arm-unknown-linux-gnueabihf ;;
        *) printf 'Unsupported Linux architecture for Kokoro.\n' >&2; exit 1 ;;
    esac
    module=$("$GO_BIN" list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)
    libs=$module/lib/$native
    install -d -m 700 -- "$data/lib"
    install -m 755 -- "$libs/libsherpa-onnx-c-api.so" "$libs/libonnxruntime.so" "$data/lib/"
    if [[ -f "$libs/libsherpa-onnx-cxx-api.so" ]]; then
        install -m 755 -- "$libs/libsherpa-onnx-cxx-api.so" "$data/lib/"
    fi
)
chmod 700 -- "$build_tmp"
mv -f -- "$build_tmp" "$bridge_bin"
trap - EXIT

mkdir -p -- "$oc_root"/{config/opencode/agents,config/opencode/plugins,data/opencode,state,cache,workspace} \
    "$config/echoear" "$units/hex.service.d"
link_file() {
    local source=$1 destination=$2
    if [[ "$destination" -ef "$source" ]]; then
        return
    fi
    if [[ -e "$destination" || -L "$destination" ]]; then
        printf 'Move the existing %s aside before running setup.\n' "$destination" >&2
        exit 1
    fi
    ln -s -- "$source" "$destination"
}
link_file "$REPO_DIR/agents/echoear.md" "$oc_root/config/opencode/agents/echoear.md"
python3 "$REPO_DIR/scripts/setup-opencode-config.py" "$oc_root/config/opencode" "$bridge_bin" "$env_file"
if [[ -f "$data_home/opencode/auth.json" ]]; then
    link_file "$data_home/opencode/auth.json" "$oc_root/data/opencode/auth.json"
fi

python3 - "$env_file" <<'PY'
import os
import re
import secrets
import sys
import tempfile
from pathlib import Path

path = Path(sys.argv[1])
old = path.read_text() if path.exists() else (
    "ECHOEAR_BRIDGE_HOST=0.0.0.0\nECHOEAR_BRIDGE_PORT=8765\n"
    f"ECHOEAR_BRIDGE_TOKEN={secrets.token_hex(32)}\n"
)
prior = re.findall(r"^ECHOEAR_OPENCODE_PASSWORD=([a-f0-9]{64})$", old, re.M)
password = prior[-1] if prior else secrets.token_hex(32)
settings = {
    "ECHOEAR_OPENCODE_URL": "http://127.0.0.1:49375",
    "ECHOEAR_OPENCODE_PASSWORD": password,
    "OPENCODE_SERVER_PASSWORD": password,
    "ECHOEAR_OPENCODE_PAIR_PORT": "49376",
}
kept = [line for line in old.splitlines(keepends=True)
        if line.strip().partition("=")[0].rstrip() not in settings]
text = "".join(kept)
if text and not text.endswith("\n"):
    text += "\n"
text += "".join(f"{key}={value}\n" for key, value in settings.items())
with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as f:
    try:
        f.write(text)
        f.close()
        os.replace(f.name, path)
    finally:
        Path(f.name).unlink(missing_ok=True)
PY

hex_path=
if [[ -z $(systemctl --user show hex.service --property=FragmentPath --value) ]]; then
    hex_path=$(type -P hex)
fi
python3 - "$REPO_DIR" "$data_home" "$bridge_bin" "$units" "$hex_path" \
    "$(type -P opencode)" "$env_file" "$oc_root" <<'PY'
import re
import sys
from pathlib import Path

def escape(value):
    return value.translate(str.maketrans({
        "\\": "\\\\", '"': '\\"', "%": "%%", "\n": "\\n", "\r": "\\r", "\t": "\\t",
    }))

repo, data, binary, directory, hex_path, opencode, env_file, oc_root = sys.argv[1:]
units = Path(directory)
paths = {"REPO_DIR": repo, "DATA_HOME": data, "BRIDGE_BIN": binary,
         "OPENCODE_BIN": opencode, "ENV_FILE": env_file}
for name in ("echoear-bridge.service", "echoear-opencode.service",
             "echoear-opencode-lan.socket", "echoear-opencode-lan.service"):
    template = Path(repo, "bridge", name).read_text()
    rendered = re.sub(r"@([A-Z_]+)@", lambda m: escape(paths[m[1]]), template)
    if name == "echoear-opencode.service":
        if Path(oc_root) != Path.home() / ".local/share/echoear/opencode":
            rendered = rendered.replace("%h/.local/share/echoear/opencode", escape(oc_root))
            rendered = rendered.replace(f"WorkingDirectory={escape(oc_root)}/workspace",
                                        f'WorkingDirectory="{escape(oc_root)}/workspace"')
        if Path(env_file) != Path.home() / ".config/echoear/bridge.env":
            rendered = rendered.replace("EnvironmentFile=%h/.config/echoear/bridge.env",
                                        f"EnvironmentFile={escape(env_file)}")
    path = units / name
    path.write_text(rendered)
    path.chmod(0o644)

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
systemctl --user enable hex.service echoear-opencode.service echoear-opencode-lan.socket echoear-bridge.service
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
systemctl --user restart echoear-opencode.service echoear-bridge.service
systemctl --user start echoear-opencode-lan.socket

# V2 stores current logins in its DB; auth.json only seeds legacy logins.
python3 - "$env_file" "$oc_root" "$(type -P opencode)" <<'PY'
import base64
import json
import os
import re
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

env_file, root, opencode = sys.argv[1:]
settings = dict(re.findall(r"^\s*(ECHOEAR_OPENCODE_\w+)\s*=([^\n]*)", Path(env_file).read_text(), re.M))
url = settings["ECHOEAR_OPENCODE_URL"]
password = settings["ECHOEAR_OPENCODE_PASSWORD"]
auth = base64.b64encode(f"opencode:{password}".encode()).decode()
client = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def get(path):
    request = urllib.request.Request(f"{url}{path}", headers={"Authorization": f"Basic {auth}"})
    with client.open(request, timeout=1) as response:
        return json.load(response)

deadline = time.monotonic() + 10
while True:
    try:
        get("/api/info")
        break
    except OSError:
        if time.monotonic() >= deadline:
            sys.exit("Dedicated OpenCode API did not become ready with Basic auth")
        time.sleep(0.2)

try:
    primary = get("/api/agent/echoear")["data"]["model"]["providerID"]
    router = next(p for entry in get("/api/config")
                  if entry.get("path") == str(Path(root, "config/opencode/opencode.jsonc"))
                  for p in entry["info"]["plugins"]
                  if isinstance(p, dict) and p.get("package") == "./echoear-model-router")
    fallback = router["options"]["fallback"].split("/", 1)[0]
except (OSError, KeyError, StopIteration, TypeError, ValueError):
    sys.exit("Could not read the EchoEar model settings from the dedicated OpenCode config")
for provider in sorted({primary, fallback}):
    if not re.fullmatch(r"[A-Za-z0-9_-][A-Za-z0-9_.-]*", provider):
        sys.exit("Invalid EchoEar model provider")
    marker = Path(root, "state", f"imported-provider-{provider}")
    if not marker.exists():
        try:
            credentials = subprocess.run([opencode, "auth", "export", provider], check=True,
                                         capture_output=True, timeout=20).stdout
            if json.loads(credentials):
                subprocess.run([opencode, "--server", url, "auth", "import"], input=credentials,
                               env={**os.environ, "OPENCODE_SERVER_PASSWORD": password}, check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
                marker.touch()
        except (OSError, ValueError, subprocess.SubprocessError):
            sys.exit("Could not import the configured providers' OpenCode credentials")
PY

printf '%s\n' 'Default voices: Heart (af_heart); Mint uses Fable (bm_fable).'
printf '%s\n' 'EchoEar MCP includes control for volume and sleep on connected devices.'
printf 'Configure firmware/sdkconfig.local as shown in README.md. The bridge token is in %s.\n' "$env_file"
