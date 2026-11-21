#!/usr/bin/env bash
set -euo pipefail
umask 077

mode=$1
data=$2
key=$3
ente=$data/bin/ente
export ENTE_CLI_HOME="$data/ente-cli"
if [[ ! -f "$key" || -L "$key" || $(stat -c '%a' -- "$key") != 600 ]]; then
    printf 'Create the Ente vault-key file with mode 0600 before exporting.\n' >&2
    exit 1
fi
exec 9>"$data/ente-photos.lock"
if ! flock -x -w 300 9; then
    printf 'Ente Photos CLI lock unavailable.\n' >&2
    exit 1
fi
vault_key="$(< "$key")"
run_ente() { ENTE_CLI_VAULT_KEY=$vault_key "$ente" "$@"; }
if [[ "$mode" == fetch ]]; then
    [[ $# == 6 && $4 =~ ^[0-9]+$ && $5 =~ ^[0-9]+$ ]] || exit 2
    run_ente account view --account personal --no-input --json |
        python3 -c 'import json,sys; a=json.load(sys.stdin); sys.exit(not (a["id"]==sys.argv[1] and a["host"]=="https://api.ente.com"))' "$4"
    ENTE_CLI_VAULT_KEY=$vault_key exec "$ente" photos file download "$5" --output "$6" --account personal --no-input --json
fi
[[ "$mode" == sync || "$mode" == sync-export ]] || exit 2

stage=$(mktemp -d "$data/.ente-sync.XXXXXXXX")
trap 'rm -rf -- "$stage"' EXIT
log=$data/ente-photos.log
: >"$log"
if ! (
    run_ente account view --account personal --no-input --json >"$stage/account" &&
    run_ente photos album list --account personal --all --no-input --json >"$stage/albums" &&
    run_ente photos file list --account personal --type image --type livephoto --all --no-input --json >"$stage/files"
) 2>>"$log"; then
    printf 'Ente Photos metadata sync failed; see the private ente-photos.log.\n' >&2
    exit 1
fi
# The city catalog is optional. Its centroids are not individual photo locations.
run_ente photos place list --account personal --all --no-input --json >"$stage/places" 2>>"$log" ||
    printf '[]\n' >"$stage/places"
if ! python3 - "$stage" <<'PY'
import json
import sys
from pathlib import Path

try:
    stage = Path(sys.argv[1])
    account = json.loads((stage / "account").read_bytes())
    assert account["name"] == "personal" and account["host"] == "https://api.ente.com"
    assert "photos" in account["apps"] and account["id"].isdigit()
    result = {"account": account["id"]}
    for name in ("albums", "files", "places"):
        result[name] = json.loads((stage / name).read_bytes())
        assert isinstance(result[name], list)
    (stage / "snapshot").write_text(json.dumps(result, ensure_ascii=False, separators=(",", ":")))
except (AssertionError, AttributeError, KeyError, OSError, TypeError, ValueError):
    sys.exit(1)
PY
then
    printf 'Ente Photos returned invalid metadata or an unexpected account.\n' >&2
    exit 1
fi
mv -fT -- "$stage/snapshot" "$data/ente-photos-ready.json"
if [[ "$mode" != sync-export ]]; then
    exit 0
fi

install -d -m 700 -- "$data/ente-export"
summary=$stage/export
links=()
if probe=$(mktemp "$data/ente-export/.link-probe.XXXXXXXX"); then
    if ln -- "$probe" "$probe.link" 2>/dev/null; then
        links=(--hardlinks)
        rm -- "$probe.link"
    fi
    rm -- "$probe"
fi
# The CLI reports private file paths on stderr. Keep them out of the journal.
if ! run_ente photos export "$data/ente-export" \
    --account personal --no-input --json --jobs 3 "${links[@]}" >"$summary" 2>>"$log"; then
    printf 'Ente Photos optional export failed; see the private ente-photos.log.\n' >&2
    exit 1
fi
if ! python3 - "$summary" "$data/ente-export" <<'PY'
import json
import sys

try:
    with open(sys.argv[1]) as f:
        result = json.load(f)
    copies, failures = result["copies"], result["failures"]
    assert result["output"] == sys.argv[2] and result["complete"] is True
    assert type(result["files"]) is int and result["files"] >= 0
    assert type(copies["expected"]) is int and copies["expected"] == copies["completed"] >= 0
    assert copies["pending"] == failures["total"] == failures["conflicts"] == 0
except (AssertionError, KeyError, OSError, TypeError, ValueError):
    sys.exit(1)
PY
then
    printf 'Ente Photos returned an invalid or incomplete export summary.\n' >&2
    exit 1
fi
mv -fT -- "$summary" "$data/ente-export-ready.json"
