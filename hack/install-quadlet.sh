#!/usr/bin/env bash
# hack/install-quadlet.sh — render the quadlet unit graph from the current
# configuration and install it where systemd (user or system scope) reads
# quadlet units, then prepare the host-side state for the shim (REQ-001,
# REQ-004, REQ-007, REQ-009, REQ-010, REQ-011).
#
# What it does:
#   1. Builds the capishim binary and renders the nine quadlet units
#      (capishim.pod + eight capishim-*.container) with the render-quadlet
#      subcommand into a temporary directory.
#   2. Installs them into ~/.config/containers/systemd/ (default) or
#      /etc/containers/systemd/ (CAPISHIM_SYSTEM=1, requires root).
#   3. Runs systemctl daemon-reload (user or system scope).
#   4. Enables lingering for the current user (user mode, best-effort; failure
#      only warns because the install itself is still valid).
#   5. Copies templates/ into ${CAPISHIM_STATE_DIR}/templates/ for
#      `clusterctl generate cluster --from` (REQ-007).
#   6. Symlinks ~/.kube/capishim.kubeconfig to the admin kubeconfig the setup
#      container writes at boot (REQ-010).
#   7. Creates the state subdirectories the containers bind-mount and writes
#      the bootstrap ABAC policy the apiserver loads (REQ-004, REQ-009).
#
# Env overrides:
#   CAPISHIM_SYSTEM        install into /etc/containers/systemd/ (requires root)
#   CAPISHIM_STATE_DIR     state directory (default ~/.local/share/capishim)
#   CAPISHIM_BIND_ADDRESS  apiserver bind address (default 127.0.0.1:6443)
#   CAPISHIM_VERSION       image tag for localhost/capishim-* images (default v0.1.0)
#
# The script is idempotent: re-running overwrites the installed units with the
# freshly rendered ones, refreshes the template copy, and re-points the
# kubeconfig symlink.

set -Eeuo pipefail
shopt -s inherit_errexit
IFS=$'\n\t'

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd -P)"

DEFAULT_STATE_DIR="${HOME}/.local/share/capishim"
DEFAULT_BIND_ADDRESS="127.0.0.1:6443"
DEFAULT_IMAGE_VERSION="v0.1.0"

CAPISHIM_STATE_DIR="${CAPISHIM_STATE_DIR:-${DEFAULT_STATE_DIR}}"
CAPISHIM_BIND_ADDRESS="${CAPISHIM_BIND_ADDRESS:-${DEFAULT_BIND_ADDRESS}}"
CAPISHIM_VERSION="${CAPISHIM_VERSION:-${DEFAULT_IMAGE_VERSION}}"

# The rendered unit set: the pod unit plus one container unit per component,
# in boot order. The renderer emits exactly these files; a mismatch fails
# loudly so drift in the component table is never masked.
UNITS=(
  capishim.pod
  capishim-pki.container
  capishim-etcd.container
  capishim-apiserver.container
  capishim-setup.container
  capishim-core.container
  capishim-cabpk.container
  capishim-kcp.container
  capishim-capd.container
)

# resolved during main(); referenced by later functions
SYSTEM_MODE="${CAPISHIM_SYSTEM:-0}"
QUADLET_DIR=""
MODE_LABEL=""
SYSTEMCTL=()
TMP_DIR=""

# ---- structured logging (stderr only) --------------------------------------
log_info() { printf '[install-quadlet] INFO: %s\n' "$*" >&2; }
log_warn() { printf '[install-quadlet] WARN: %s\n' "$*" >&2; }
log_error() { printf '[install-quadlet] ERROR: %s\n' "$*" >&2; }

# ---- cleanup on exit --------------------------------------------------------
cleanup() {
  local exit_code=$?
  if [[ -n "${TMP_DIR}" && -d "${TMP_DIR}" ]]; then
    rm -rf -- "${TMP_DIR}"
  fi
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
  for cmd in go systemctl; do
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
# choose_target_dir — resolve the quadlet install directory and the systemctl
# daemon-reload invocation for the selected mode; system mode requires root
# ---------------------------------------------------------------------------
choose_target_dir() {
  local system_mode="${1}"
  if [[ "${system_mode}" == "1" ]]; then
    if (( EUID != 0 )); then
      log_error "CAPISHIM_SYSTEM=1 installs into /etc/containers/systemd/ and requires root"
      log_error "re-run as root: sudo CAPISHIM_SYSTEM=1 make install-quadlet"
      exit 1
    fi
    QUADLET_DIR="/etc/containers/systemd"
    SYSTEMCTL=(systemctl)
    MODE_LABEL="system"
  else
    QUADLET_DIR="${HOME}/.config/containers/systemd"
    SYSTEMCTL=(systemctl --user)
    MODE_LABEL="user"
  fi
}

# ---------------------------------------------------------------------------
# render_units — build the capishim binary and render the unit set into ${1}
# ---------------------------------------------------------------------------
render_units() {
  local render_dir="${1}"
  local binary="${TMP_DIR}/capishim"
  log_info "building capishim renderer (go build ./cmd/capishim)"
  (cd "${REPO_ROOT}" && go build -o "${binary}" ./cmd/capishim)
  log_info "rendering quadlet units: state=${CAPISHIM_STATE_DIR} bind=${CAPISHIM_BIND_ADDRESS} version=${CAPISHIM_VERSION}"
  CAPISHIM_STATE_DIR="${CAPISHIM_STATE_DIR}" \
    CAPISHIM_BIND_ADDRESS="${CAPISHIM_BIND_ADDRESS}" \
    CAPISHIM_VERSION="${CAPISHIM_VERSION}" \
    "${binary}" render-quadlet --dir "${render_dir}"
}

# ---------------------------------------------------------------------------
# verify_units — every expected unit exists in ${1} and nothing else
# ---------------------------------------------------------------------------
verify_units() {
  local render_dir="${1}"
  local -a rendered=()
  local unit
  mapfile -t rendered < <(find "${render_dir}" -maxdepth 1 -type f \( -name 'capishim.pod' -o -name 'capishim-*.container' \))
  if (( ${#rendered[@]} != ${#UNITS[@]} )); then
    log_error "renderer produced ${#rendered[@]} units, expected ${#UNITS[@]}"
    return 1
  fi
  for unit in "${UNITS[@]}"; do
    if [[ ! -f "${render_dir}/${unit}" ]]; then
      log_error "renderer did not produce ${unit}"
      return 1
    fi
  done
}

# ---------------------------------------------------------------------------
# install_units — copy the rendered units into the quadlet directory
# ---------------------------------------------------------------------------
install_units() {
  local render_dir="${1}"
  local unit
  mkdir -p -- "${QUADLET_DIR}"
  for unit in "${UNITS[@]}"; do
    install -m 0644 -- "${render_dir}/${unit}" "${QUADLET_DIR}/${unit}"
  done
  log_info "installed ${#UNITS[@]} quadlet units into ${QUADLET_DIR}"
}

# ---------------------------------------------------------------------------
# reload_daemon — ask systemd to re-read the quadlet directory
# ---------------------------------------------------------------------------
reload_daemon() {
  log_info "reloading systemd daemon (${MODE_LABEL} mode)"
  "${SYSTEMCTL[@]}" daemon-reload
}

# ---------------------------------------------------------------------------
# enable_linger — keep user units running after logout (user mode only,
# best-effort; failure warns because the install itself is still valid)
# ---------------------------------------------------------------------------
enable_linger() {
  if [[ "${SYSTEM_MODE}" == "1" ]]; then
    return 0
  fi
  if ! command -v loginctl >/dev/null 2>&1; then
    log_warn "loginctl not found; skipping linger (user units stop after logout)"
    return 0
  fi
  local uid
  uid="$(id -u)"
  if ! loginctl enable-linger "${uid}" >/dev/null 2>&1; then
    log_warn "loginctl enable-linger ${uid} failed; user units stop after logout"
    return 0
  fi
  log_info "enabled linger for uid ${uid}"
}

# ---------------------------------------------------------------------------
# install_templates — copy templates/ into the state dir for
# `clusterctl generate cluster --from`
# ---------------------------------------------------------------------------
install_templates() {
  local src="${REPO_ROOT}/templates"
  local dst="${CAPISHIM_STATE_DIR}/templates"
  if [[ ! -d "${src}" ]]; then
    log_error "templates directory missing: ${src} (run make vendor-templates first)"
    return 1
  fi
  mkdir -p -- "${dst}"
  cp -a -- "${src}/." "${dst}/"
  log_info "copied templates into ${dst}"
}

# ---------------------------------------------------------------------------
# install_kubeconfig_symlink — point ~/.kube/capishim.kubeconfig at the admin
# kubeconfig the setup container writes at boot (REQ-010)
# ---------------------------------------------------------------------------
install_kubeconfig_symlink() {
  local kube_dir="${HOME}/.kube"
  local admin="${CAPISHIM_STATE_DIR}/kubeconfigs/admin.kubeconfig"
  local link="${kube_dir}/capishim.kubeconfig"
  mkdir -p -- "${kube_dir}"
  ln -sf -- "${admin}" "${link}"
  log_info "symlinked ${link} -> ${admin}"
}

# ---------------------------------------------------------------------------
# prepare_state_dirs — create the state subdirectories the quadlet containers
# bind-mount and write the bootstrap ABAC policy the apiserver loads, so a
# clean host that runs only `make install-quadlet` boots without the state
# prep the e2e shim performs (REQ-004, REQ-009, VC-01)
# ---------------------------------------------------------------------------
prepare_state_dirs() {
  local -a dirs=(
    "${CAPISHIM_STATE_DIR}/pki"
    "${CAPISHIM_STATE_DIR}/etcd"
    "${CAPISHIM_STATE_DIR}/kubeconfigs"
    "${CAPISHIM_STATE_DIR}/abac"
    "${CAPISHIM_STATE_DIR}/pki/core-webhook"
    "${CAPISHIM_STATE_DIR}/pki/cabpk-webhook"
    "${CAPISHIM_STATE_DIR}/pki/kcp-webhook"
    "${CAPISHIM_STATE_DIR}/pki/capd-webhook"
  )
  local dir
  for dir in "${dirs[@]}"; do
    mkdir -p -- "${dir}"
  done
  log_info "prepared state directories under ${CAPISHIM_STATE_DIR}"

  local policy_path="${CAPISHIM_STATE_DIR}/abac/policy.json"
  local policy='{"apiVersion":"abac.authorization.kubernetes.io/v1beta1","kind":"Policy","spec":{"user":"capishim:admin","namespace":"*","resource":"*","apiGroup":"*","nonResourcePath":"*"}}'
  printf '%s\n' "${policy}" > "${policy_path}"
  chmod 0644 -- "${policy_path}"
  log_info "wrote ABAC policy ${policy_path}"
}

# ---------------------------------------------------------------------------
# print_summary — next commands for the operator
# ---------------------------------------------------------------------------
print_summary() {
  local start_cmd
  if [[ "${SYSTEM_MODE}" == "1" ]]; then
    start_cmd="sudo systemctl start capishim-pod.service"
  else
    start_cmd="systemctl --user start capishim-pod.service"
  fi
  printf '\n'
  printf 'capishim quadlet units installed (%s mode): %s\n' "${MODE_LABEL}" "${QUADLET_DIR}"
  printf 'Next steps:\n'
  printf '  %s\n' "${start_cmd}"
  printf '  KUBECONFIG=%s kubectl get clusters\n' "${HOME}/.kube/capishim.kubeconfig"
  printf '  clusterctl generate cluster <name> --from %s/templates/cluster-template-in-memory.yaml \\\n' "${CAPISHIM_STATE_DIR}"
  printf '    --kubernetes-version v1.36.1 --control-plane-machine-count 3 --worker-machine-count 3 | kubectl apply -f -\n'
  printf '\n'
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  check_deps
  choose_target_dir "${SYSTEM_MODE}"

  TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/capishim-install-quadlet.XXXXXX")"
  local render_dir="${TMP_DIR}/render"
  mkdir -p -- "${render_dir}"

  render_units "${render_dir}"
  verify_units "${render_dir}"
  install_units "${render_dir}"
  reload_daemon
  enable_linger
  install_templates
  install_kubeconfig_symlink
  prepare_state_dirs
  print_summary
}

main "$@"
