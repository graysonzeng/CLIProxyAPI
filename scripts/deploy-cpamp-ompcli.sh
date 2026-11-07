#!/usr/bin/env bash
# Upgrade CPA-Manager-Plus on ompcli.
# Default: latest GitHub release tarball (linux_amd64).
# Source build: LOCAL_TGZ=/path/to/cpa-manager-plus_<tag>_linux_amd64.tar.gz TAG=<tag>
# Never runs install-cpamp.sh. Auto-rollback on health failure. Preserves config.json + data/.
# Usage: ./scripts/deploy-cpamp-ompcli.sh [--check] [--force]
set -euo pipefail
SSH_HOST="${SSH_HOST:-ompcli}"
FORCE=0
CHECK=0
for a in "$@"; do
  case "$a" in
    --force) FORCE=1 ;;
    --check) CHECK=1 ;;
  esac
done
if [[ -n "${LOCAL_TGZ:-}" ]]; then
  [[ -f "$LOCAL_TGZ" ]] || { echo "LOCAL_TGZ not found: $LOCAL_TGZ"; exit 1; }
  TAG="${TAG:-$(basename "$LOCAL_TGZ" | sed -n 's/^cpa-manager-plus_\(.*\)_linux_amd64\.tar\.gz$/\1/p')}"
  [[ -n "$TAG" ]] || { echo "cannot derive TAG from LOCAL_TGZ; set TAG="; exit 1; }
else
  TAG="${TAG:-$(curl -fsS https://api.github.com/repos/seakee/CPA-Manager-Plus/releases/latest | python3 -c 'import json,sys;print(json.load(sys.stdin)["tag_name"])')}"
fi
CUR=$(ssh -o BatchMode=yes "$SSH_HOST" "systemctl show -p ExecStart --value cpa-manager-plus | grep -o 'cpa-manager-plus_v[^/]*_linux_amd64' | head -1 | sed 's/cpa-manager-plus_//;s/_linux_amd64//'")
echo "[cpamp] current=$CUR latest=$TAG local=${LOCAL_TGZ:-}"
if [[ "$CHECK" == 1 ]]; then
  [[ "$CUR" == "$TAG" ]] && exit 0 || exit 1
fi
if [[ "$CUR" == "$TAG" && "$FORCE" != 1 && -z "${LOCAL_TGZ:-}" ]]; then
  echo "[cpamp] up to date; skip (use --force or LOCAL_TGZ to redeploy)"
  exit 0
fi
PKG=cpa-manager-plus_${TAG}_linux_amd64
if [[ -n "${LOCAL_TGZ:-}" ]]; then
  scp -o BatchMode=yes "$LOCAL_TGZ" "$SSH_HOST:/root/cpa-manager-plus/downloads/$PKG.tar.gz"
fi
ssh -o BatchMode=yes "$SSH_HOST" env TAG="$TAG" CUR="$CUR" PKG="$PKG" HAS_LOCAL="${LOCAL_TGZ:+1}" bash -s <<'EOS'
set -euo pipefail
B=/root/cpa-manager-plus
OLD=cpa-manager-plus_${CUR}_linux_amd64
UNIT=/etc/systemd/system/cpa-manager-plus.service
TS=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p $B/downloads $B/runtime $B/backups
cd $B/downloads
if [[ -z "${HAS_LOCAL:-}" ]]; then
  curl -fsSL -o $PKG.tar.gz https://github.com/seakee/CPA-Manager-Plus/releases/download/$TAG/$PKG.tar.gz
fi
rm -rf $B/runtime/$PKG.tmp $B/runtime/$PKG
mkdir $B/runtime/$PKG.tmp
tar -xzf $PKG.tar.gz -C $B/runtime/$PKG.tmp
D=$(dirname "$(find $B/runtime/$PKG.tmp -name cpa-manager-plus -type f | head -1)")
[[ -x "$D/cpa-manager-plus" ]] || { echo "binary not found in tarball"; exit 1; }
mv "$D" $B/runtime/$PKG
rm -rf $B/runtime/$PKG.tmp
cp -a $B/runtime/$OLD/config.json $B/runtime/$PKG/config.json
cp -a $UNIT $B/backups/cpa-manager-plus.service.$TS.$CUR
cp -a $B/run.sh $B/backups/run.sh.$TS.$CUR
systemctl stop cpa-manager-plus
cp -a $B/data $B/backups/data.$TS.$CUR
rollback() { echo "ROLLBACK to $CUR" >&2; cp -a $B/backups/cpa-manager-plus.service.$TS.$CUR $UNIT; cp -a $B/backups/run.sh.$TS.$CUR $B/run.sh; systemctl daemon-reload; systemctl restart cpa-manager-plus; exit 1; }
python3 - <<PY
from pathlib import Path
old="$OLD"; new="$PKG"
for p in ["$UNIT", "$B/run.sh"]:
    path=Path(p); text=path.read_text()
    if old not in text:
        raise SystemExit(f"old package not found in {p}: {old}")
    path.write_text(text.replace(old, new))
    print("updated", p)
PY
systemctl daemon-reload
systemctl restart cpa-manager-plus
sleep 4
systemctl is-active --quiet cpa-manager-plus || rollback
code=$(curl -s -o /dev/null -w '%{http_code}' -m 8 http://127.0.0.1:18317/)
echo "local_http=$code"
[[ "$code" =~ ^(200|301|302|307|401)$ ]] || rollback
curl -fsS -m 8 http://127.0.0.1:18317/health >/dev/null || rollback
journalctl -u cpa-manager-plus -n 8 --no-pager | tail -8
echo "OK $CUR -> $TAG; data backup $B/backups/data.$TS.$CUR"
echo "ROLLBACK_CMD: cp -a $B/backups/cpa-manager-plus.service.$TS.$CUR $UNIT && cp -a $B/backups/run.sh.$TS.$CUR $B/run.sh && systemctl daemon-reload && systemctl restart cpa-manager-plus"
EOS
