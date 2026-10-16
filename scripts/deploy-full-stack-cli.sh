#!/usr/bin/env bash
# DEFAULT production update/deploy for this workspace (API + Management UI).
# Preflight + build BOTH repos, write a paired manifest, then deploy API then UI.
# Avoids half-upgraded production when UI build fails after API restart.
#
# Agents: when the user says 同步/更新/构建/部署/验证 without “only API/UI”,
# run THIS script (prefer --sync). Do not default to deploy-cli.sh alone.
#
# Usage:
#   ./scripts/deploy-full-stack-cli.sh --sync       # DEFAULT for community update
#   ./scripts/deploy-full-stack-cli.sh              # build current HEADs + deploy
#   ./scripts/deploy-full-stack-cli.sh --dry-run
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MGMT_ROOT="${MGMT_ROOT:-/Users/sheng/tencent/Cli-Proxy-API-Management-Center}"
MANIFEST_DIR="${MANIFEST_DIR:-${ROOT}/dist}"
SSH_HOST="${SSH_HOST:-cli}"

DO_SYNC=0
DRY_RUN=0

usage() {
  cat <<'EOF'
DEFAULT full-stack deploy: CLIProxyAPI + Management UI
(preflight+build both, then deploy). See docs/ops-update-deploy.md / AGENTS.md.

Usage:
  ./scripts/deploy-full-stack-cli.sh --sync       # default for 同步/更新
  ./scripts/deploy-full-stack-cli.sh              # current HEADs, no pull
  ./scripts/deploy-full-stack-cli.sh --dry-run
Env: ALLOW_DIRTY SKIP_PANEL_CHECK SSH_HOST MGMT_ROOT
Opt-in single-side only: scripts/deploy-cli.sh | deploy-management-cli.sh
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sync) DO_SYNC=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 1 ;;
  esac
  shift
done

log() { printf '[deploy-full] %s\n' "$*"; }
die() { printf '[deploy-full] ERROR: %s\n' "$*" >&2; exit 1; }

API_SCRIPT="${ROOT}/scripts/deploy-cli.sh"
UI_SCRIPT="${ROOT}/scripts/deploy-management-cli.sh"
[[ -x "$API_SCRIPT" ]] || die "missing $API_SCRIPT"
[[ -x "$UI_SCRIPT" ]] || die "missing $UI_SCRIPT"
[[ -d "$MGMT_ROOT/.git" ]] || die "MGMT_ROOT not a git repo: $MGMT_ROOT"

build_args=()
if [[ "$DO_SYNC" -eq 1 ]]; then
  build_args+=(--sync)
fi
build_args+=(--build-only)
if [[ "$DRY_RUN" -eq 1 ]]; then
  build_args+=(--dry-run)
fi

log "Phase 1/3: preflight + build API (no deploy)"
"$API_SCRIPT" "${build_args[@]}"

log "Phase 2/3: preflight + build Management UI (no deploy)"
"$UI_SCRIPT" "${build_args[@]}"

API_BIN="${ROOT}/dist/cli-proxy-api-linux-amd64"
UI_HTML="${MGMT_ROOT}/dist/index.html"

if [[ "$DRY_RUN" -eq 1 ]]; then
  log "DRY-RUN: skip deploy phases and manifest write"
  exit 0
fi

[[ -f "$API_BIN" ]] || die "API binary missing after build"
[[ -f "$UI_HTML" ]] || die "UI html missing after build"

API_SHA="$(sha256sum "$API_BIN" | awk '{print $1}')"
UI_SHA="$(sha256sum "$UI_HTML" | awk '{print $1}')"
API_VER="$(git -C "$ROOT" describe --tags --always)"
API_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
UI_VER="$(git -C "$MGMT_ROOT" describe --tags --always)"
UI_COMMIT="$(git -C "$MGMT_ROOT" rev-parse HEAD)"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$MANIFEST_DIR"
MANIFEST="${MANIFEST_DIR}/full-stack-manifest-${TS}.txt"

{
  echo "created_at=${TS}"
  echo "ssh_host=${SSH_HOST}"
  echo "api_version=${API_VER}"
  echo "api_commit=${API_COMMIT}"
  echo "api_sha256=${API_SHA}"
  echo "api_artifact=${API_BIN}"
  echo "ui_version=${UI_VER}"
  echo "ui_commit=${UI_COMMIT}"
  echo "ui_sha256=${UI_SHA}"
  echo "ui_artifact=${UI_HTML}"
} | tee "$MANIFEST"

log "Manifest: $MANIFEST"
log "Phase 3/3: deploy API then UI"
"$API_SCRIPT" --deploy-only
"$UI_SCRIPT" --deploy-only

# Append backup hints from latest remote listing (best-effort)
{
  echo "deployed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "status=ok"
} >> "$MANIFEST"

log "Full stack deploy complete."
log "Manifest: $MANIFEST"
log "API ${API_VER} ${API_SHA:0:12}… | UI ${UI_VER} ${UI_SHA:0:12}…"
