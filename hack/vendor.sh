#!/usr/bin/env bash
# hack/vendor.sh — vendor in-memory templates and rendered provider manifests
# from upstream cluster-api at the pinned release tag (REQ-007, REQ-011).
#
# What it does:
#   1. Resolves the upstream tag from ${CAPI_SOURCE_REF} (default v1.14.0).
#      The tag must exist upstream; a missing tag fails loudly (non-zero exit)
#      so pin drift is never masked by an automatic fallback.
#   2. Shallow-clones upstream at the resolved tag into .vendor-tmp (removed on
#      exit).
#   3. Copies the two in-memory template files verbatim into templates/.
#   4. Renders each provider's config/default with kustomize v5 (pinned via
#      ${KUSTOMIZE_VERSION}, default v5.6.0) into
#      templates/manifests/<provider>/provider.yaml (full rendered output;
#      kind filtering is the internal/manifests task's concern).
#   5. Writes templates/VENDORED.md with provenance: tag, commit, kustomize
#      version, per-artifact source paths, timestamp.
#
# Env overrides:
#   CAPI_SOURCE_REF   upstream tag to vendor from (default v1.14.0)
#   KUSTOMIZE_VERSION kustomize v5 version to pin (default v5.6.0)
#
# The script is idempotent: re-running against the same pin produces
# byte-identical templates and manifests, and leaves VENDORED.md untouched
# (its original timestamp is preserved).

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

UPSTREAM_REPO="https://github.com/kubernetes-sigs/cluster-api.git"
DEFAULT_CAPI_SOURCE_REF="v1.14.0"
DEFAULT_KUSTOMIZE_VERSION="v5.6.0"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"

TEMPLATES_DIR="${REPO_ROOT}/templates"
MANIFESTS_DIR="${TEMPLATES_DIR}/manifests"
TMP_DIR="${REPO_ROOT}/.vendor-tmp"
UPSTREAM_SRC="${TMP_DIR}/cluster-api"

# provider entries: name|upstream kustomization dir (from repo root)|namespace|namePrefix
declare -a PROVIDERS
PROVIDERS=(
  "core|core/config/default|capi-system|capi-"
  "cabpk|bootstrap/kubeadm/config/default|capi-kubeadm-bootstrap-system|capi-kubeadm-bootstrap-"
  "kcp|controlplane/kubeadm/config/default|capi-kubeadm-control-plane-system|capi-kubeadm-control-plane-"
  "capd|test/infrastructure/docker/config/default|capd-system|capd-"
)

CAPI_SOURCE_REF="${CAPI_SOURCE_REF:-${DEFAULT_CAPI_SOURCE_REF}}"
KUSTOMIZE_VERSION="${KUSTOMIZE_VERSION:-${DEFAULT_KUSTOMIZE_VERSION}}"

# resolved during main(); referenced by later functions
RESOLVED_REF=""
COMMIT_SHA=""

# ---- structured logging (stderr only) --------------------------------------
log_info() { printf '[vendor-templates] INFO: %s\n' "$*" >&2; }
log_warn() { printf '[vendor-templates] WARN: %s\n' "$*" >&2; }
log_error() { printf '[vendor-templates] ERROR: %s\n' "$*" >&2; }

# ---- cleanup on exit --------------------------------------------------------
cleanup() {
  local exit_code=$?
  rm -rf -- "${TMP_DIR}"
  trap - EXIT ERR
  exit "${exit_code}"
}
trap cleanup EXIT
trap 'log_error "Command failed at line ${LINENO}: ${BASH_COMMAND}"' ERR

# ---------------------------------------------------------------------------
# check_deps — verify required tooling is present
# ---------------------------------------------------------------------------
check_deps() {
  local -a missing=()
  local cmd
  for cmd in git go; do
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
# resolve_tag — verify ${1} exists upstream; fail loudly if it does not so pin
# drift (e.g. a typo or an upstream tag that was never released) is never
# masked by an automatic fallback. Prints the resolved tag.
# ---------------------------------------------------------------------------
resolve_tag() {
  local requested="${1}"
  local refs

  if ! refs="$(git ls-remote --tags "${UPSTREAM_REPO}" "refs/tags/${requested}" 2>&1)"; then
    log_error "cannot reach upstream repository ${UPSTREAM_REPO}; verify network access"
    return 1
  fi
  if grep -q "refs/tags/${requested}$" <<< "${refs}"; then
    log_info "upstream tag ${requested} exists"
    printf '%s\n' "${requested}"
    return 0
  fi

  log_error "requested tag ${requested} does not exist upstream (${UPSTREAM_REPO}); refusing to vendor from an unknown tag"
  log_error "set CAPI_SOURCE_REF to an existing upstream tag (e.g. CAPI_SOURCE_REF=v1.14.0 make vendor-templates) and re-run"
  return 1
}

# ---------------------------------------------------------------------------
# copy_templates — copy the two in-memory template files verbatim into templates/
# ---------------------------------------------------------------------------
copy_templates() {
  local -a srcs dsts
  srcs=(
    "${UPSTREAM_SRC}/test/infrastructure/docker/templates/cluster-template-in-memory.yaml"
    "${UPSTREAM_SRC}/test/infrastructure/docker/templates/clusterclass-in-memory.yaml"
  )
  dsts=(
    "${TEMPLATES_DIR}/cluster-template-in-memory.yaml"
    "${TEMPLATES_DIR}/clusterclass-in-memory.yaml"
  )
  local i src
  for i in "${!srcs[@]}"; do
    src="${srcs[$i]}"
    if [[ ! -f "${src}" ]]; then
      log_error "upstream template missing at pin ${RESOLVED_REF}: ${src}"
      return 1
    fi
    install -m 0644 -- "${src}" "${dsts[$i]}"
  done
  log_info "copied in-memory templates verbatim into ${TEMPLATES_DIR}"
}

# ---------------------------------------------------------------------------
# render_manifests — kustomize-render each provider's config/default
# ---------------------------------------------------------------------------
render_manifests() {
  local entry name src_path ns prefix out
  for entry in "${PROVIDERS[@]}"; do
    IFS='|' read -r name src_path ns prefix <<< "${entry}"
    if [[ ! -d "${UPSTREAM_SRC}/${src_path}" ]]; then
      log_error "upstream kustomization dir missing at pin ${RESOLVED_REF}: ${src_path}"
      return 1
    fi
    out="${MANIFESTS_DIR}/${name}/provider.yaml"
    mkdir -p -- "$(dirname -- "${out}")"
    log_info "rendering ${name} manifests (${src_path}) with kustomize ${KUSTOMIZE_VERSION}"
    if ! go run "sigs.k8s.io/kustomize/kustomize/v5@${KUSTOMIZE_VERSION}" build "${UPSTREAM_SRC}/${src_path}" > "${out}"; then
      log_error "kustomize ${KUSTOMIZE_VERSION} failed to render ${name} manifests"
      log_error "if ${KUSTOMIZE_VERSION} is unavailable in the module cache/network, set KUSTOMIZE_VERSION to the latest v5 tag and re-run (VENDORED.md records the version used)"
      return 1
    fi
    if [[ ! -s "${out}" ]]; then
      log_error "kustomize produced empty output for ${name}"
      return 1
    fi
  done
  log_info "rendered provider manifests into ${MANIFESTS_DIR}"
}

# ---------------------------------------------------------------------------
# render_vendored_md — build VENDORED.md content (${1} = timestamp)
# ---------------------------------------------------------------------------
render_vendored_md() {
  local ts="${1}"
  local provider_rows=""
  local entry name src_path ns prefix
  for entry in "${PROVIDERS[@]}"; do
    IFS='|' read -r name src_path ns prefix <<< "${entry}"
    provider_rows+="| \`manifests/${name}/provider.yaml\` | \`${src_path}\` | namespace \`${ns}\`, namePrefix \`${prefix}\` |"$'\n'
  done
  printf '%s' "\
# Vendored upstream content

This directory vendors content from upstream
[kubernetes-sigs/cluster-api](https://github.com/kubernetes-sigs/cluster-api)
at a pinned release tag. Every file in this directory is copied verbatim from
the upstream tag and must never be hand-edited. Changes happen deliberately via
\`make vendor-templates\` after bumping the upstream pin.

## Pin

- Upstream tag: \`${RESOLVED_REF}\`
- Upstream commit: \`${COMMIT_SHA}\`
- kustomize version: \`${KUSTOMIZE_VERSION}\` (invoked as \`go run sigs.k8s.io/kustomize/kustomize/v5@${KUSTOMIZE_VERSION} build\`)
- Vendored at: ${ts} (UTC)

## In-memory templates (REQ-007)

| File | Upstream source (at pin) |
|---|---|
| \`cluster-template-in-memory.yaml\` | \`test/infrastructure/docker/templates/cluster-template-in-memory.yaml\` |
| \`clusterclass-in-memory.yaml\` | \`test/infrastructure/docker/templates/clusterclass-in-memory.yaml\` |

The ClusterClass file is self-contained: every referenced template
(DevClusterTemplate, KubeadmControlPlaneTemplate, DevMachineTemplate,
KubeadmConfigTemplate) is defined inline in the same file, so exactly these two
files are vendored.

## Rendered provider manifests

Each \`provider.yaml\` is the full output of \`kustomize build\` over the
upstream provider's \`config/default\` (namespace + namePrefix kustomization),
including Deployment, Service, ServiceAccount, and cert-manager objects. The
setup container (\`internal/manifests\`) filters to the kinds it applies
(Namespace, CustomResourceDefinition, ClusterRole, ClusterRoleBinding, Role,
RoleBinding, ValidatingWebhookConfiguration, MutatingWebhookConfiguration).

| Provider file | Upstream kustomization source (at pin) | kustomization |
|---|---|---|
${provider_rows}
## Rules

- Do not hand-edit any file in this directory; upstream content is vendored verbatim.
- Pin consistency is enforced by \`make check-pins\` (REQ-013, VC-10).
"
}

# ---------------------------------------------------------------------------
# write_vendored_md — write VENDORED.md, preserving the original file when the
# pin data (tag, commit, kustomize version, source paths) is unchanged so
# re-runs are idempotent
# ---------------------------------------------------------------------------
write_vendored_md() {
  local file="${TEMPLATES_DIR}/VENDORED.md"
  local rendered existing normalized
  rendered="$(render_vendored_md "$(date -u +'%Y-%m-%dT%H:%M:%SZ')")"
  if [[ -f "${file}" ]]; then
    existing="$(grep -v '^- Vendored at: ' "${file}" || true)"
    normalized="$(grep -v '^- Vendored at: ' <<< "${rendered}" || true)"
    if [[ "${existing}" == "${normalized}" ]]; then
      log_info "VENDORED.md already up to date (pin unchanged); preserving original timestamp"
      return 0
    fi
  fi
  printf '%s' "${rendered}" > "${file}"
  log_info "wrote ${file}"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  check_deps

  log_info "resolving upstream tag (CAPI_SOURCE_REF=${CAPI_SOURCE_REF})"
  RESOLVED_REF="$(resolve_tag "${CAPI_SOURCE_REF}")"
  log_info "vendoring from upstream tag ${RESOLVED_REF}"

  log_info "cloning upstream (shallow) into ${UPSTREAM_SRC}"
  git clone --depth 1 --branch "${RESOLVED_REF}" --single-branch -- "${UPSTREAM_REPO}" "${UPSTREAM_SRC}"
  COMMIT_SHA="$(git -C "${UPSTREAM_SRC}" rev-parse HEAD)"
  log_info "cloned commit ${COMMIT_SHA}"

  copy_templates
  render_manifests
  write_vendored_md

  log_info "done: vendored ${RESOLVED_REF} (${COMMIT_SHA}) into ${TEMPLATES_DIR}"
}

main "$@"
