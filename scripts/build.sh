#!/usr/bin/env bash
set -euo pipefail

firmware=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../firmware" && pwd -P)
if [[ ! -f "$firmware/sdkconfig.local" ]]; then
    echo "Copy firmware/sdkconfig.local.example to firmware/sdkconfig.local and fill in your credentials." >&2
    exit 1
fi

if command -v podman >/dev/null 2>&1; then
    podman=(podman)
elif command -v host-spawn >/dev/null 2>&1; then
    podman=(host-spawn podman)
else
    echo "Podman is required to build the firmware." >&2
    exit 1
fi

exec "${podman[@]}" run --rm --user 0 \
    -v "$firmware:/project:Z" -w /project \
    docker.io/espressif/idf:release-v5.5 \
    bash -ec '
        umask 077
        # Kconfig defaults do not override an existing sdkconfig.
        rm -f build/sdkconfig
        idf.py -B build -D SDKCONFIG=build/sdkconfig \
            -D "SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.local" reconfigure build
    '
