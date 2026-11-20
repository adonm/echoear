# shellcheck shell=bash

resolve_go() {
    local candidate path
    local -a candidates
    if [[ -n ${GO_BIN:-} ]]; then
        candidates=("$GO_BIN")
    else
        local mise_data=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}
        mapfile -d '' -t candidates < <(printf '%s\0' "$mise_data"/installs/go/*/bin/go | sort -zVr)
        candidates=(go "${candidates[@]}")
    fi
    for candidate in "${candidates[@]}"; do
        if path=$(type -P -- "$candidate") && "$path" version >/dev/null 2>&1; then
            [[ $path = /* ]] || path="$PWD/$path"
            printf '%s\n' "$path"
            return
        fi
    done
    printf 'Install Go 1.26 or set GO_BIN to a working go executable.\n' >&2
    return 1
}
