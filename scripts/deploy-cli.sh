#!/usr/bin/env bash
# API-binary ONLY deploy helper for SSH host "cli".
# NOT the workspace default — default is scripts/deploy-full-stack-cli.sh
# (API + Management UI). Use this only when the user explicitly wants backend-only,
# or when invoked by deploy-full-stack-cli.sh.
# See docs/ops-update-deploy.md.
#
# Usage:
#   ./scripts/deploy-cli.sh              # clean build HEAD + deploy + hard verify
#   ./scripts/deploy-cli.sh --sync       # pull origin/main (clean-tree), then build+deploy
#   ./scripts/deploy-cli.sh --build-only
#   ./scripts/deploy-cli.sh --deploy-only
#   ./scripts/deploy-cli.sh --dry-run
#   ALLOW_DIRTY=1 ./scripts/deploy-cli.sh   # skip clean-tree / use in-place dirty build (not for prod)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SSH_HOST="${SSH_HOST:-cli}"
REMOTE_DIR="${REMOTE_DIR:-/root/cliproxyapi}"
REMOTE_BIN="${REMOTE_DIR}/cli-proxy-api"
REMOTE_NEW="${REMOTE_DIR}/cli-proxy-api.new"
LOCAL_BIN="${LOCAL_BIN:-${ROOT}/dist/cli-proxy-api-linux-amd64}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:36275/healthz}"
PUBLIC_HEALTH_URL="${PUBLIC_HEALTH_URL:-https://cli.688663.xyz/healthz}"
MODELS_URL="${MODELS_URL:-http://127.0.0.1:36275/v1/models}"
SERVICE="${SERVICE:-cliproxyapi.service}"
ALLOW_DIRTY="${ALLOW_DIRTY:-0}"
CONFIG_PATH="${CONFIG_PATH:-${REMOTE_DIR}/config.yaml}"

DO_SYNC=0
DO_BUILD=1
DO_DEPLOY=1
DRY_RUN=0

usage() {
  cat <<'EOF'
API-binary ONLY helper (not the default full-stack path).
Default for 更新/部署: ./scripts/deploy-full-stack-cli.sh --sync
See docs/ops-update-deploy.md.

Usage:
  ./scripts/deploy-cli.sh              # clean build HEAD + deploy + hard verify
  ./scripts/deploy-cli.sh --sync       # git pull --ff-only origin/main, then build+deploy
  ./scripts/deploy-cli.sh --build-only
  ./scripts/deploy-cli.sh --deploy-only
  ./scripts/deploy-cli.sh --dry-run
Env: SSH_HOST REMOTE_DIR LOCAL_BIN HEALTH_URL PUBLIC_HEALTH_URL MODELS_URL SERVICE ALLOW_DIRTY=1
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

log() { printf '[deploy-cli] %s\n' "$*"; }
die() { printf '[deploy-cli] ERROR: %s\n' "$*" >&2; exit 1; }

run() {
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "DRY-RUN: $*"
  else
    "$@"
  fi
}

# --sync must not fight local tracked edits. Untracked ops files (docs/, scripts/, *.json) are OK.
require_tracked_clean_for_sync() {
  if [[ "$ALLOW_DIRTY" == "1" ]]; then
    log "ALLOW_DIRTY=1: skipping tracked-clean gate for --sync"
    return 0
  fi
  if ! git diff --quiet || ! git diff --cached --quiet; then
    die "tracked working tree is dirty; commit/stash before --sync, or ALLOW_DIRTY=1"
  fi
}

warn_if_worktree_dirty() {
  if [[ -n "$(git status --porcelain)" ]]; then
    log "WARN: local worktree is dirty; production build uses committed HEAD only (git worktree), not uncommitted edits"
  fi
}

if [[ "$DO_SYNC" -eq 1 ]]; then
  log "Sync origin/main (ff-only)"
  require_tracked_clean_for_sync
  run git fetch origin --tags
  run git checkout main
  run git pull --ff-only origin main
fi

VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD)"
COMMIT_FULL="$(git rev-parse HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TS="$(date +%Y%m%dT%H%M%S)"
REMOTE_BAK="${REMOTE_DIR}/cli-proxy-api.backup.${TS}.${COMMIT}"

log "VERSION=${VERSION} COMMIT=${COMMIT} BUILD_DATE=${BUILD_DATE}"
log "branch=$(git rev-parse --abbrev-ref HEAD) head=${COMMIT_FULL}"

if [[ "$DO_BUILD" -eq 1 ]]; then
  warn_if_worktree_dirty
  mkdir -p "$(dirname "$LOCAL_BIN")"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    log "DRY-RUN: git worktree + go build ..."
  elif [[ "$ALLOW_DIRTY" == "1" ]]; then
    log "ALLOW_DIRTY=1: in-place build (may be +dirty)"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -ldflags "-s -w -X main.Version=${VERSION}-dirty -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
      -o "${LOCAL_BIN}" \
      ./cmd/server
    command -v file >/dev/null && file "$LOCAL_BIN" || true
    sha256sum "$LOCAL_BIN" | tee "${LOCAL_BIN}.sha256"
  else
    # Always compile committed HEAD in an isolated worktree so local dirt cannot taint the binary.
    log "Cross-compile linux/amd64 → ${LOCAL_BIN} (clean worktree of ${COMMIT})"
    WT=""
    cleanup_wt() {
      if [[ -n "${WT}" && -d "${WT}" ]]; then
        git -C "$ROOT" worktree remove --force "${WT}" 2>/dev/null || rm -rf "${WT}"
      fi
    }
    trap cleanup_wt EXIT
    WT="$(mktemp -d "${TMPDIR:-/tmp}/cliproxy-build.XXXXXX")"
    git worktree add --detach "${WT}" "${COMMIT_FULL}"
    (
      cd "${WT}"
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -buildvcs=true \
        -ldflags "-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILD_DATE}" \
        -o "${LOCAL_BIN}" \
        ./cmd/server
    )
    cleanup_wt
    trap - EXIT
    command -v file >/dev/null && file "$LOCAL_BIN" || true
    sha256sum "$LOCAL_BIN" | tee "${LOCAL_BIN}.sha256"
    if go version -m "$LOCAL_BIN" 2>/dev/null | grep -qE '\+dirty|vcs.modified=true'; then
      die "built binary still reports dirty VCS metadata; refuse prod deploy"
    fi
    {
      echo "version=${VERSION}"
      echo "commit=${COMMIT_FULL}"
      echo "build_date=${BUILD_DATE}"
      echo "sha256=$(sha256sum "$LOCAL_BIN" | awk '{print $1}')"
      echo "build_mode=clean_worktree"
    } > "${LOCAL_BIN}.meta"
  fi
fi

if [[ "$DO_DEPLOY" -eq 0 ]]; then
  log "Build-only done."
  exit 0
fi

[[ -f "$LOCAL_BIN" ]] || die "missing binary: $LOCAL_BIN (run without --deploy-only first)"

if [[ "$DRY_RUN" -eq 1 ]]; then
  LOCAL_SHA="dry-run"
else
  LOCAL_SHA="$(sha256sum "$LOCAL_BIN" | awk '{print $1}')"
fi
log "LOCAL_SHA=${LOCAL_SHA}"

log "Upload → ${SSH_HOST}:${REMOTE_NEW}"
run scp -o ConnectTimeout=20 -o BatchMode=yes "$LOCAL_BIN" "${SSH_HOST}:${REMOTE_NEW}"

log "Remote backup/replace/restart + hard verify"
if [[ "$DRY_RUN" -eq 1 ]]; then
  log "DRY-RUN: remote install + systemctl restart ${SERVICE}"
  exit 0
fi

ssh -o ConnectTimeout=20 -o BatchMode=yes "$SSH_HOST" \
  env REMOTE_DIR="$REMOTE_DIR" REMOTE_NEW="$REMOTE_NEW" REMOTE_BIN="$REMOTE_BIN" \
      REMOTE_BAK="$REMOTE_BAK" LOCAL_SHA="$LOCAL_SHA" SERVICE="$SERVICE" \
      HEALTH_URL="$HEALTH_URL" MODELS_URL="$MODELS_URL" \
      EXPECT_VERSION="$VERSION" EXPECT_COMMIT="$COMMIT" CONFIG_PATH="$CONFIG_PATH" \
  bash -s <<'EOS'
set -euo pipefail

fail() {
  echo "ERROR: $*" >&2
  if [[ "${INSTALLED:-0}" == "1" && -f "$REMOTE_BAK" ]]; then
    cp -a "$REMOTE_BAK" "$REMOTE_BIN" && systemctl restart "$SERVICE" && echo "AUTO_ROLLBACK=restored $REMOTE_BAK" >&2
  fi
  exit 1
}

NEW_SHA=$(sha256sum "$REMOTE_NEW" | awk '{print $1}')
echo "NEW_SHA=$NEW_SHA expected=$LOCAL_SHA"
[[ "$NEW_SHA" == "$LOCAL_SHA" ]] || fail "sha256 mismatch after upload"

BANNER=$("$REMOTE_NEW" -version 2>&1 | head -n 1 || true)
echo "BANNER=$BANNER"
echo "$BANNER" | grep -Fq "Version: ${EXPECT_VERSION}" || fail "banner missing Version: ${EXPECT_VERSION}"
echo "$BANNER" | grep -Fq "Commit: ${EXPECT_COMMIT}" || fail "banner missing Commit: ${EXPECT_COMMIT}"

# Backup currently running binary; sidecar must describe the BACKUP contents.
if [[ -f "$REMOTE_BIN" ]]; then
  OLD_SHA=$(sha256sum "$REMOTE_BIN" | awk '{print $1}')
  cp -a "$REMOTE_BIN" "$REMOTE_BAK"
  echo "${OLD_SHA}  $(basename "$REMOTE_BAK")" > "${REMOTE_BAK}.sha256"
  {
    echo "backup_of_path=${REMOTE_BIN}"
    echo "backup_sha256=${OLD_SHA}"
    echo "replaced_by_sha256=${LOCAL_SHA}"
    echo "replaced_by_version=${EXPECT_VERSION}"
    echo "replaced_by_commit=${EXPECT_COMMIT}"
    echo "created_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  } > "${REMOTE_BAK}.meta"
  echo "BACKUP=$REMOTE_BAK OLD_SHA=$OLD_SHA"
else
  echo "WARN: no existing binary to backup"
fi

install -m 0755 "$REMOTE_NEW" "$REMOTE_BIN"
INSTALLED=1
rm -f "$REMOTE_NEW"
BIN_SHA=$(sha256sum "$REMOTE_BIN" | awk '{print $1}')
[[ "$BIN_SHA" == "$LOCAL_SHA" ]] || fail "installed binary sha mismatch"

systemctl restart "$SERVICE"
sleep 3
systemctl is-active "$SERVICE" >/dev/null || fail "service not active"
active=$(systemctl is-active "$SERVICE")
echo "service=$active"

MAIN_PID=$(systemctl show -p MainPID --value "$SERVICE")
[[ -n "$MAIN_PID" && "$MAIN_PID" != "0" ]] || fail "MainPID missing"
echo "MainPID=$MAIN_PID"
EXE_SHA=$(sha256sum "/proc/${MAIN_PID}/exe" | awk '{print $1}')
echo "EXE_SHA=$EXE_SHA BIN_SHA=$BIN_SHA"
[[ "$EXE_SHA" == "$LOCAL_SHA" ]] || fail "running exe sha != deployed binary"

HZ=$(curl -fsS -m 8 "$HEALTH_URL" || true)
echo "healthz=$HZ"
[[ "$HZ" == '{"status":"ok"}' ]] || fail "healthz not ok"

# Unauthenticated models must reject (auth gate alive).
MODELS_CODE=$(curl -s -o /tmp/models-deploy-verify.json -w "%{http_code}" -m 8 "$MODELS_URL" || true)
echo "models_http=$MODELS_CODE"
[[ "$MODELS_CODE" == "401" || "$MODELS_CODE" == "403" ]] || fail "expected 401/403 on unauthenticated /v1/models, got $MODELS_CODE"

# Recent journal should mention the expected version string.
if ! journalctl -u "$SERVICE" -n 50 --no-pager | grep -Fq "Version: ${EXPECT_VERSION}, Commit: ${EXPECT_COMMIT}"; then
  fail "journal missing expected Version/Commit banner"
fi
journalctl -u "$SERVICE" -n 15 --no-pager || true

echo "ROLLBACK=cp -a ${REMOTE_BAK} ${REMOTE_BIN} && systemctl restart ${SERVICE}"
echo "BACKUP=$REMOTE_BAK"
EOS

log "Public health (best-effort)"
curl -fsS -m 10 "$PUBLIC_HEALTH_URL" && echo || log "public health probe failed (non-fatal)"

log "Done. VERSION=${VERSION} COMMIT=${COMMIT} SHA256=${LOCAL_SHA} BACKUP=${REMOTE_BAK}"
log "Rollback: ssh ${SSH_HOST} 'cp -a ${REMOTE_BAK} ${REMOTE_BIN} && systemctl restart ${SERVICE}'"
