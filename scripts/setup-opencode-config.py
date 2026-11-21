#!/usr/bin/env python3
"""Install EchoEar's OpenCode config without overwriting user model selections."""

import hashlib
import json
import os
import re
import sys
import tempfile
from pathlib import Path

directory, bridge, env_file = map(Path, sys.argv[1:])
source = Path(__file__).resolve().parent.parent / "config/opencode/echoear-fallback.js"
config = directory / "opencode.jsonc"
legacy = directory / "plugins/echoear-model-router.js"
package = "./echoear-model-router"


def put(path, content):
    if path.exists() and path.read_bytes() == content:
        return
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as tmp:
        try:
            tmp.write(content)
            tmp.close()
            os.replace(tmp.name, path)
        finally:
            Path(tmp.name).unlink(missing_ok=True)


if legacy.exists() and hashlib.sha256(legacy.read_bytes()).hexdigest() != (
    "3584853ab040775e27ee7c53864a44e78a202d25e69f7d892f31eb8016e3e81c"
):
    sys.exit("Move the customized legacy EchoEar router out of the OpenCode plugins directory before upgrading.")

update = None
if not config.exists() or legacy.exists():
    # Only the old setup-owned file needs migration; never rewrite the user's
    # JSONC after migration, including when they change models.
    try:
        update = json.loads(config.read_text()) if config.exists() else {
            "$schema": "https://opencode.ai/config.json",
            "default_agent": "echoear",
            "mcp": {"servers": {"echoear": {"type": "local", "command": [str(bridge), "mcp"]}}},
        }
    except ValueError:
        sys.exit("Could not migrate the dedicated OpenCode JSONC; move the config aside, run setup, and merge your settings.")
    previous = re.findall(r"^\s*ECHOEAR_OPENCODE_MODEL\s*=([^\n]*)", env_file.read_text(), re.M) if env_file.exists() else []
    model = previous[-1].strip().strip("'\"") if previous else ""
    update.setdefault("agents", {}).setdefault("echoear", {}).setdefault("model", model or "opencode/gemini-4-hydrogen#low")
    plugins = update.setdefault("plugins", [])
    if not any(isinstance(p, dict) and p.get("package") == package for p in plugins):
        plugins.append({"package": package, "options": {"fallback": "opencode/claude-haiku-5-5"}})

router = directory / package
router.mkdir(mode=0o700, parents=True, exist_ok=True)
put(router / "index.js", source.read_bytes())
if update is not None:
    legacy.unlink(missing_ok=True)
    put(config, (json.dumps(update, indent=2) + "\n").encode())
