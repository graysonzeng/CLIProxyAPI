#!/usr/bin/env bash
# ompcli API + Management UI deploy. CPAMP is documented, not automated.
# See docs/ops-3repo-deploy-ompcli.md
#
# Usage:
#   ./scripts/deploy-ompcli-3repo.sh           # current HEADs
#   ./scripts/deploy-ompcli-3repo.sh --sync    # ff-only origin/main in both repos first
#   ./scripts/deploy-ompcli-3repo.sh --dry-run
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export SSH_HOST="${SSH_HOST:-ompcli}"
export HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:18080/healthz}"
export MODELS_URL="${MODELS_URL:-http://127.0.0.1:18080/v1/models}"
export PUBLIC_HEALTH_URL="${PUBLIC_HEALTH_URL:-https://c.688663.xyz/healthz}"
export MGMT_URL="${MGMT_URL:-http://127.0.0.1:18080/management.html}"
export PUBLIC_MGMT_URL="${PUBLIC_MGMT_URL:-https://c.688663.xyz/management.html}"
export MGMT_ROOT="${MGMT_ROOT:-/Users/sheng/tencent/Cli-Proxy-API-Management-Center}"

API_SCRIPT="${ROOT}/scripts/deploy-cli.sh"
UI_SCRIPT="${ROOT}/scripts/deploy-management-cli.sh"
SNAP_DIR="${TMPDIR:-/tmp}/ompcli-oauth-snap-$$"
mkdir -p "$SNAP_DIR"
cleanup() { rm -rf "$SNAP_DIR"; }
trap cleanup EXIT

log() { printf '[deploy-ompcli] %s\n' "$*"; }
die() { printf '[deploy-ompcli] ERROR: %s\n' "$*" >&2; exit 1; }

[[ -x "$API_SCRIPT" ]] || die "missing $API_SCRIPT"
[[ -x "$UI_SCRIPT" ]] || die "missing $UI_SCRIPT"

SYNC_ARGS=()
DRY_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --sync) SYNC_ARGS+=(--sync) ;;
    --dry-run) DRY_ARGS+=(--dry-run) ;;
    -h|--help)
      sed -n '1,12p' "$0"
      exit 0
      ;;
    *) die "unknown option: $arg" ;;
  esac
done

snapshot_oauth() {
  local out="$1"
  ssh -o ConnectTimeout=20 -o BatchMode=yes "$SSH_HOST" python3 - <<'PY' >"$out"
import hashlib, json, os
from urllib.parse import urlsplit

d = "/root/.cli-proxy-api"
print("CONFIG", hashlib.sha256(open("/root/cliproxyapi/config.yaml", "rb").read()).hexdigest())
for name in sorted(os.listdir(d)):
    path = os.path.join(d, name)
    if os.path.isdir(path) or not name.endswith(".json"):
        continue
    raw = open(path, "rb").read()
    digest = hashlib.sha256(raw).hexdigest()[:16]
    data = json.loads(raw.decode("utf-8"))
    proxy = data.get("proxy_url") or data.get("proxy-url") or ""
    host = "NONE"
    if proxy:
        u = urlsplit(proxy)
        host = "%s://%s" % (u.scheme, u.hostname)
        if u.port:
            host += ":%s" % u.port
    print("FILE", name, "size="+str(len(raw)), "sha="+digest, "proxy="+host)
PY
}

log "host=${SSH_HOST} health=${HEALTH_URL} public=${PUBLIC_HEALTH_URL}"
log "OAuth fingerprint snapshot (desensitized; no userinfo)"
snapshot_oauth "${SNAP_DIR}/before.txt"
cat "${SNAP_DIR}/before.txt"

log "Phase 1/2: CLIProxyAPI binary"
"$API_SCRIPT" "${SYNC_ARGS[@]}" "${DRY_ARGS[@]}"

if [[ ${#DRY_ARGS[@]} -eq 0 ]]; then
  snapshot_oauth "${SNAP_DIR}/after.txt"
  if ! cmp -s "${SNAP_DIR}/before.txt" "${SNAP_DIR}/after.txt"; then
    printf '[deploy-ompcli] OAuth/config fingerprint drift:\n' >&2
    diff -u "${SNAP_DIR}/before.txt" "${SNAP_DIR}/after.txt" >&2 || true
    die "config.yaml or OAuth proxy fingerprints changed; stop. See docs/ops-3repo-deploy-ompcli.md and /root/cliproxyapi/AGENTS.md"
  fi
  log "OAuth/config fingerprints unchanged"
fi

log "Phase 2/2: Management Center HTML"
"$UI_SCRIPT" "${SYNC_ARGS[@]}" "${DRY_ARGS[@]}"

log "API + UI done on ${SSH_HOST}."
log "CPA-Manager-Plus is NOT deployed by this script."
log "Next: docs/ops-3repo-deploy-ompcli.md §3 (native systemd switch) and §4 (cleanup-derived if required)."
log "Do not run bin/install-cpamp.sh on ompcli."
