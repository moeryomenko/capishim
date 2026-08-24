#!/usr/bin/env bash
# hack/update-hypervisor-manifests.sh — refresh the vendored hypervisor provider
# manifests from a cluster-api-hypervisor (CAPH) checkout (REQ-001).
#
# What it does:
#   1. Resolves the CAPH checkout directory from ${1} (default:
#      ../cluster-api-hypervisor relative to this repository).
#   2. Runs `make components` there to build the clusterctl release tree under
#      its out/ directory.
#   3. Copies the three component files into
#      templates/manifests/<provider>-hypervisor/provider.yaml. A missing or
#      empty source file fails the run with a message naming every missing
#      file.
#
# The script is idempotent: re-running against an unchanged CAPH checkout
# produces byte-identical vendored files, so a second run is a no-op diff.
#
# Usage: hack/update-hypervisor-manifests.sh [path-to-caph-checkout]
#
# Env overrides:
#   OUT_DIR         release tree directory inside the CAPH checkout, relative
#                   to it (default out)
#   RELEASE_VERSION version directory under each provider folder
#                   (default v0.1.0)

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

DEFAULT_CAPH_DIR="../cluster-api-hypervisor"
DEFAULT_OUT_DIR="out"
DEFAULT_RELEASE_VERSION="v0.1.0"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"

MANIFESTS_DIR="${REPO_ROOT}/templates/manifests"

CAPH_DIR="${1:-${REPO_ROOT}/${DEFAULT_CAPH_DIR}}"
OUT_DIR="${OUT_DIR:-${DEFAULT_OUT_DIR}}"
RELEASE_VERSION="${RELEASE_VERSION:-${DEFAULT_RELEASE_VERSION}}"

# Vendored trees as "directory|component file" pairs; each component file is
# copied to <directory>/provider.yaml.
declare -a TREES=(
  "infrastructure-hypervisor|infrastructure-components.yaml"
  "bootstrap-hypervisor|bootstrap-components.yaml"
  "control-plane-hypervisor|control-plane-components.yaml"
)

log_info() { printf '[update-hypervisor-manifests] INFO: %s\n' "$*" >&2; }
log_error() { printf '[update-hypervisor-manifests] ERROR: %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# check_deps — verify required tooling is present
# ---------------------------------------------------------------------------
check_deps() {
  local -a missing=()
  local cmd
  for cmd in go make; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
      missing+=("${cmd}")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "missing required tools: ${missing[*]}"
    return 1
  fi
}

# ---------------------------------------------------------------------------
# build_release_tree — run `make components` inside the CAPH checkout
# ---------------------------------------------------------------------------
build_release_tree() {
  if [[ ! -f "${CAPH_DIR}/Makefile" ]]; then
    log_error "cluster-api-hypervisor checkout not found: ${CAPH_DIR}"
    log_error "pass the checkout path as the first argument, e.g.: hack/update-hypervisor-manifests.sh ../cluster-api-hypervisor"
    return 1
  fi
  log_info "building release tree: make -C ${CAPH_DIR} components OUT_DIR=${OUT_DIR} RELEASE_VERSION=${RELEASE_VERSION}"
  make -C "${CAPH_DIR}" components OUT_DIR="${OUT_DIR}" RELEASE_VERSION="${RELEASE_VERSION}"
}

# ---------------------------------------------------------------------------
# copy_manifests — copy the three component files into the vendored trees,
# failing loudly when any source file is missing or empty
# ---------------------------------------------------------------------------
copy_manifests() {
  local -a missing=()
  local entry tree src
  for entry in "${TREES[@]}"; do
    IFS='|' read -r tree src <<< "${entry}"
    src="${CAPH_DIR}/${OUT_DIR}/${tree}/${RELEASE_VERSION}/${src}"
    if [[ ! -s "${src}" ]]; then
      missing+=("${src}")
    fi
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "release tree incomplete after 'make components'; run it inside ${CAPH_DIR}:"
    local path
    for path in "${missing[@]}"; do
      log_error "  missing or empty: ${path}"
    done
    return 1
  fi

  for entry in "${TREES[@]}"; do
    IFS='|' read -r tree src <<< "${entry}"
    mkdir -p -- "${MANIFESTS_DIR}/${tree}"
    install -m 0644 -- "${CAPH_DIR}/${OUT_DIR}/${tree}/${RELEASE_VERSION}/${src}" \
      "${MANIFESTS_DIR}/${tree}/provider.yaml"
    log_info "vendored ${tree}/provider.yaml"
  done
}

main() {
  check_deps
  build_release_tree
  copy_manifests
  log_info "done: hypervisor manifests refreshed from ${CAPH_DIR} (${RELEASE_VERSION})"
}

main "$@"
