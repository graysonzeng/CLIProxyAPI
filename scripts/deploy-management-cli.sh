#!/usr/bin/env bash
# Management UI ONLY deploy helper for SSH host "cli".
# NOT the workspace default — default is scripts/deploy-full-stack-cli.sh
# (API + Management UI). Use this only when the user explicitly wants frontend-only,
# or when invoked by deploy-full-stack-cli.sh.
# See docs/ops-update-deploy.md § Management UI.
#
# Toolchain: Bun (packageManager) + frozen bun.lock.
# Usage:
#   ./scripts/deploy-management-cli.sh              # build HEAD + deploy + verify
#   ./scripts/deploy-management-cli.sh --sync       # pull origin/main, then build+deploy
#   ./scripts/deploy-management-cli.sh --build-only
#   ./scripts/deploy-management-cli.sh --deploy-only
#   ./scripts/deploy-management-cli.sh --dry-run
set -euo pipefail

MGMT_ROOT="${MGMT_ROOT:-/Users/sheng/tencent/Cli-Proxy-API-Management-Center}"
SSH_HOST="${SSH_HOST:-cli}"
REMOTE_STATIC="${REMOTE_STATIC:-/root/cliproxyapi/static}"
REMOTE_FILE="${REMOTE_STATIC}/management.html"
REMOTE_NEW="${REMOTE_STATIC}/management.html.new"
REMOTE_CONFIG="${REMOTE_CONFIG:-/root/cliproxyapi/config.yaml}"
LOCAL_HTML="${LOCAL_HTML:-${MGMT_ROOT}/dist/index.html}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:36275/healthz}"
MGMT_URL="${MGMT_URL:-http://127.0.0.1:36275/management.html}"
PUBLIC_MGMT_URL="${PUBLIC_MGMT_URL:-https://cli.688663.xyz/management.html}"
ALLOW_DIRTY="${ALLOW_DIRTY:-0}"
SKIP_PANEL_CHECK="${SKIP_PANEL_CHECK:-0}"

DO_SYNC=0
DO_BUILD=1
DO_DEPLOY=1
DRY_RUN=0

usage() {
  cat <<'EOF'
Management UI ONLY helper (not the default full-stack path).
Default for 更新/部署: ./scripts/deploy-full-stack-cli.sh --sync

Usage:
  ./scripts/deploy-management-cli.sh              # bun frozen build + deploy + verify
  ./scripts/deploy-management-cli.sh --sync       # git pull --ff-only origin/main, then build+deploy
  ./scripts/deploy-management-cli.sh --build-only
  ./scripts/deploy-management-cli.sh --deploy-only
  ./scripts/deploy-management-cli.sh --dry-run
Env: MGMT_ROOT SSH_HOST REMOTE_STATIC LOCAL_HTML ALLOW_DIRTY=1 SKIP_PANEL_CHECK=1
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --sync) DO_SYNC=1 ;;
    --build-only) DO_BUILD=1; DO_DEPLOY=0 ;;
    --deploy-only) DO_BUILD=0; DO_DEPLOY=1 ;;
    --dry-run) DRY_RUN=1 ;;
    -h|--help) usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 1 ;;
  esac
  shift
done

log() { printf '[deploy-mgmt] %s\n' "$*"; }
die() { printf '[deploy-mgmt] ERROR: %s\n' "$*" >&2; exit 1; }
run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then log "DRY-RUN: $*"; else "$@"; fi
}

require_clean() {
  if [[ "$ALLOW_DIRTY" == "1" ]]; then
    log "ALLOW_DIRTY=1: skipping clean-tree gate"
    return 0
  fi
  if [[ -n "$(git status --porcelain)" ]]; then
    die "working tree dirty; stash/commit first, or ALLOW_DIRTY=1"
  fi
}

[[ -d "$MGMT_ROOT/.git" ]] || die "MGMT_ROOT not a git repo: $MGMT_ROOT"
cd "$MGMT_ROOT"

if [[ "$DO_SYNC" -eq 1 ]]; then
  log "Sync origin/main (ff-only)"
  require_clean
  run git fetch origin --tags
  run git checkout main
  run git pull --ff-only origin main
fi

VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD)"
COMMIT_FULL="$(git rev-parse HEAD)"
TS="$(date +%Y%m%dT%H%M%S)"
REMOTE_BAK="${REMOTE_STATIC}/management.html.backup.${TS}.${COMMIT}"

log "VERSION=${VERSION} COMMIT=${COMMIT} branch=$(git rev-parse --abbrev-ref HEAD)"

if [[ "$DO_BUILD" -eq 1 ]]; then
  # Drop npm lock residue so it cannot trip dirty gates or diverge from bun.lock
  rm -f package-lock.json
  require_clean
  command -v bun >/dev/null 2>&1 || die "bun not found (packageManager is bun); install bun or fix PATH"
  [[ -f bun.lock ]] || die "bun.lock missing; cannot frozen install"
  log "bun install --frozen-lockfile + bun run build → dist/index.html"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "DRY-RUN: bun install --frozen-lockfile && bun run build"
  else
    bun install --frozen-lockfile
    bun run build
    ls -lh "$LOCAL_HTML"
    sha256sum "$LOCAL_HTML" | tee "${LOCAL_HTML}.sha256"
    {
      echo "version=${VERSION}"
      echo "commit=${COMMIT_FULL}"
      echo "sha256=$(sha256sum "$LOCAL_HTML" | awk '{print $1}')"
      echo "tool=bun"
    } > "${LOCAL_HTML}.meta"
  fi
fi

if [[ "$DO_DEPLOY" -eq 0 ]]; then
  log "Build-only done."
  exit 0
fi

[[ -f "$LOCAL_HTML" ]] || die "missing $LOCAL_HTML (build first)"

if [[ "$DRY_RUN" -eq 1 ]]; then
  LOCAL_SHA="dry-run"
else
  LOCAL_SHA="$(sha256sum "$LOCAL_HTML" | awk '{print $1}')"
fi
log "LOCAL_SHA=${LOCAL_SHA}"
log "Upload → ${SSH_HOST}:${REMOTE_NEW}"
run scp -o ConnectTimeout=20 -o BatchMode=yes "$LOCAL_HTML" "${SSH_HOST}:${REMOTE_NEW}"

if [[ "$DRY_RUN" -eq 1 ]]; then
  log "DRY-RUN: remote install to ${REMOTE_FILE}"
  exit 0
fi

ssh -o ConnectTimeout=20 -o BatchMode=yes "$SSH_HOST" \
  env REMOTE_STATIC="$REMOTE_STATIC" REMOTE_FILE="$REMOTE_FILE" REMOTE_NEW="$REMOTE_NEW" \
      REMOTE_BAK="$REMOTE_BAK" LOCAL_SHA="$LOCAL_SHA" VERSION="$VERSION" COMMIT="$COMMIT" \
      HEALTH_URL="$HEALTH_URL" MGMT_URL="$MGMT_URL" REMOTE_CONFIG="$REMOTE_CONFIG" \
      SKIP_PANEL_CHECK="$SKIP_PANEL_CHECK" \
  bash -s <<'EOS'
set -euo pipefail
fail() { echo "ERROR: $*" >&2; exit 1; }

mkdir -p "$REMOTE_STATIC"

# Hard precheck: manual pin is meaningless if auto-updater can overwrite the file.
if [[ "${SKIP_PANEL_CHECK}" != "1" ]]; then
  if [[ -f "$REMOTE_CONFIG" ]]; then
    if ! grep -Eq '^[[:space:]]*disable-auto-update-panel:[[:space:]]*true[[:space:]]*(#.*)?$' "$REMOTE_CONFIG"; then
      fail "config missing disable-auto-update-panel: true (set SKIP_PANEL_CHECK=1 to override)"
    fi
    echo "panel_auto_update=disabled (ok)"
  else
    echo "WARN: config not found at $REMOTE_CONFIG; skip panel auto-update check"
  fi
fi

NEW_SHA=$(sha256sum "$REMOTE_NEW" | awk '{print $1}')
echo "NEW_SHA=$NEW_SHA expected=$LOCAL_SHA"
[[ "$NEW_SHA" == "$LOCAL_SHA" ]] || fail "sha256 mismatch after upload"

if [[ -f "$REMOTE_FILE" ]]; then
  OLD_SHA=$(sha256sum "$REMOTE_FILE" | awk '{print $1}')
  cp -a "$REMOTE_FILE" "$REMOTE_BAK"
  echo "${OLD_SHA}  $(basename "$REMOTE_BAK")" > "${REMOTE_BAK}.sha256"
  {
    echo "backup_of_path=${REMOTE_FILE}"
    echo "backup_sha256=${OLD_SHA}"
    echo "replaced_by_sha256=${LOCAL_SHA}"
    echo "replaced_by_version=${VERSION}"
    echo "replaced_by_commit=${COMMIT}"
    echo "created_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "${REMOTE_BAK}.meta"
  echo "BACKUP=$REMOTE_BAK OLD_SHA=$OLD_SHA"
fi

install -m 0644 "$REMOTE_NEW" "$REMOTE_FILE"
rm -f "$REMOTE_NEW"
FILE_SHA=$(sha256sum "$REMOTE_FILE" | awk '{print $1}')
[[ "$FILE_SHA" == "$LOCAL_SHA" ]] || fail "installed management.html sha mismatch"

curl -fsS -m 8 "$HEALTH_URL" | grep -Fq '"status":"ok"' || fail "healthz not ok"

code=$(curl -s -o /tmp/mgmt-verify.html -w "%{http_code}" -m 20 "$MGMT_URL")
echo "MGMT_HTTP=$code"
[[ "$code" == "200" ]] || fail "management.html HTTP $code"
SERVED_SHA=$(sha256sum /tmp/mgmt-verify.html | awk '{print $1}')
echo "SERVED_SHA=$SERVED_SHA"
[[ "$SERVED_SHA" == "$LOCAL_SHA" ]] || fail "served content sha mismatch"
grep -qiE '<!doctype html|<html' /tmp/mgmt-verify.html || fail "served body not HTML"
wc -c /tmp/mgmt-verify.html

echo "ROLLBACK=cp -a ${REMOTE_BAK} ${REMOTE_FILE}"
EOS

log "Public probe (best-effort, large file may be slow via tunnel)"
curl -fsS -m 90 -o /tmp/mgmt-public-verify.html -w "public_http=%{http_code} size=%{size_download}\n" \
  "$PUBLIC_MGMT_URL" && sha256sum /tmp/mgmt-public-verify.html || log "public probe failed (non-fatal if local 200+sha ok)"

log "Done. VERSION=${VERSION} COMMIT=${COMMIT} SHA256=${LOCAL_SHA} BACKUP=${REMOTE_BAK}"
log "Rollback: ssh ${SSH_HOST} 'cp -a ${REMOTE_BAK} ${REMOTE_FILE}'"
