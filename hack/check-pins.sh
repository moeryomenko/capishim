#!/usr/bin/env bash
# hack/check-pins.sh — verify upstream cluster-api pins agree (REQ-013, VC-10).
#
# The upstream pin is recorded in three independent places and all must name
# the same v1.14.x tag:
#
#   1. e2e/go.mod — the sigs.k8s.io/cluster-api require version plus the
#      replace directives for the root, api and test modules (the api/test
#      placeholder versions are resolved by those replaces).
#   2. Makefile CAPI_SOURCE_REF — the build arg fed to the Containerfiles by
#      `make images` and to hack/vendor.sh by `make vendor-templates`.
#   3. templates/VENDORED.md — the "Upstream tag:" provenance line written by
#      hack/vendor.sh.
#
# Exits 0 when every source agrees and prints "pins agree: <tag>"; exits 1
# with a per-source diff otherwise. Used by `make check-pins` (VC-10).

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"

E2E_GOMOD="${REPO_ROOT}/e2e/go.mod"
MAKEFILE="${REPO_ROOT}/Makefile"
VENDORED_MD="${REPO_ROOT}/templates/VENDORED.md"

fail() {
    printf 'ERROR: %s\n' "$*" >&2
    exit 1
}

# go_mod_require_version — version of the root cluster-api module declared in
# the require block of e2e/go.mod. The replace lines and the api/test
# placeholder requires never match this pattern (they are followed by "=>",
# "/api" or "/test", never by a bare version).
go_mod_require_version() {
    awk 'match($0, /^[[:space:]]*sigs\.k8s\.io\/cluster-api[[:space:]]+v[0-9]/) { print $NF; exit }' "$E2E_GOMOD"
}

# go_mod_replace_version — version that the replace directive for MODULE
# (cluster-api, cluster-api/api or cluster-api/test) pins e2e/go.mod to.
go_mod_replace_version() {
    local module="$1"
    awk -v mod="sigs.k8s.io/${module}" \
        'match($0, "^[[:space:]]*" mod "[[:space:]]*=>") { print $NF; exit }' "$E2E_GOMOD"
}

# makefile_source_ref — the CAPI_SOURCE_REF value in the Makefile.
makefile_source_ref() {
    awk '/^CAPI_SOURCE_REF[[:space:]]*\?*=/ { print $NF; exit }' "$MAKEFILE"
}

# vendored_upstream_tag — the tag recorded in templates/VENDORED.md.
vendored_upstream_tag() {
    sed -n 's/^[-*][[:space:]]*Upstream tag:[[:space:]]*`\(v[0-9][^`]*\)`.*/\1/p' "$VENDORED_MD" | head -1
}

[[ -f "$E2E_GOMOD" ]] || fail "e2e/go.mod not found: ${E2E_GOMOD}"
[[ -f "$MAKEFILE" ]] || fail "Makefile not found: ${MAKEFILE}"
[[ -f "$VENDORED_MD" ]] || fail "templates/VENDORED.md not found: ${VENDORED_MD}"

# Each entry is "label|version". The labels describe the pin source so a
# mismatch message points at the exact file and line construct to fix.
declare -a SOURCES
SOURCES=(
    "e2e/go.mod (require sigs.k8s.io/cluster-api)|$(go_mod_require_version)"
    "e2e/go.mod (replace sigs.k8s.io/cluster-api)|$(go_mod_replace_version cluster-api)"
    "e2e/go.mod (replace sigs.k8s.io/cluster-api/api)|$(go_mod_replace_version cluster-api/api)"
    "e2e/go.mod (replace sigs.k8s.io/cluster-api/test)|$(go_mod_replace_version cluster-api/test)"
    "Makefile CAPI_SOURCE_REF|$(makefile_source_ref)"
    "templates/VENDORED.md Upstream tag|$(vendored_upstream_tag)"
)

for entry in "${SOURCES[@]}"; do
    label="${entry%%|*}"
    value="${entry#*|}"
    [[ -n "$value" ]] || fail "could not extract the upstream pin from: ${label}"
done

reference="${SOURCES[0]#*|}"
mismatch=false
for entry in "${SOURCES[@]}"; do
    label="${entry%%|*}"
    value="${entry#*|}"
    if [[ "$value" != "$reference" ]]; then
        printf '  %-58s %s (expected %s)\n' "$label" "$value" "$reference"
        mismatch=true
    fi
done
if [[ "$mismatch" == true ]]; then
    printf 'ERROR: upstream pins disagree; bump CAPI_SOURCE_REF first, then run make vendor-templates\n' >&2
    exit 1
fi

printf 'pins agree: %s\n' "$reference"
