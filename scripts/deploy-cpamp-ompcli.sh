#!/usr/bin/env bash
# Upgrade CPA-Manager-Plus on ompcli to latest upstream release (linux_amd64) per
# /root/cpa-manager-plus/DEPLOY.md. Never runs install-cpamp.sh. Auto-rollback on failure.
# Usage: ./scripts/deploy-cpamp-ompcli.sh [--check]   (TAG=vX.Y.Z to pin)
set -euo pipefail
SSH_HOST="${SSH_HOST:-ompcli}"
TAG="${TAG:-$(curl -fsS https://api.github.com/repos/seakee/CPA-Manager-Plus/releases/latest | python3 -c 'import json,sys;print(json.load(sys.stdin)["tag_name"])')}"
CUR=$(ssh -o BatchMode=yes "$SSH_HOST" "systemctl show -p ExecStart --value cpa-manager-plus | grep -o 'cpa-manager-plus_v[0-9.]*_linux_amd64' | head -1 | sed 's/cpa-manager-plus_//;s/_linux_amd64//'")
echo "[cpamp] current=$CUR latest=$TAG"
[[ "$CUR" == "$TAG" ]] && { echo "[cpamp] up to date; skip"; exit 0; }
[[ "${1:-}" == "--check" ]] && exit 0
ssh -o BatchMode=yes "$SSH_HOST" env TAG="$TAG" CUR="$CUR" bash -s <<'EOS'
set -euo pipefail
B=/root/cpa-manager-plus; PKG=cpa-manager-plus_${TAG}_linux_amd64; OLD=cpa-manager-plus_${CUR}_linux_amd64
UNIT=/etc/systemd/system/cpa-manager-plus.service; TS=$(date -u +%Y%m%dT%H%M%SZ)
mkdir -p $B/downloads $B/runtime $B/backups
cd $B/downloads
curl -fsSL -o $PKG.tar.gz https://github.com/seakee/CPA-Manager-Plus/releases/download/$TAG/$PKG.tar.gz
rm -rf $B/runtime/$PKG.tmp && mkdir $B/runtime/$PKG.tmp && tar -xzf $PKG.tar.gz -C $B/runtime/$PKG.tmp
D=$(dirname "$(find $B/runtime/$PKG.tmp -name cpa-manager-plus -type f | head -1)")
[[ -x "$D/cpa-manager-plus" ]] || { echo "binary not found in tarball"; exit 1; }
rm -rf $B/runtime/$PKG; mv "$D" $B/runtime/$PKG; rm -rf $B/runtime/$PKG.tmp
cp -a $B/runtime/$OLD/config.json $B/runtime/$PKG/config.json
cp -a $UNIT $B/backups/cpa-manager-plus.service.$TS.$CUR; cp -a $B/run.sh $B/backups/run.sh.$TS.$CUR
systemctl stop cpa-manager-plus; cp -a $B/data $B/backups/data.$TS.$CUR
rollback() { echo "ROLLBACK to $CUR" >&2; cp -a $B/backups/cpa-manager-plus.service.$TS.$CUR $UNIT; cp -a $B/backups/run.sh.$TS.$CUR $B/run.sh; systemctl daemon-reload; systemctl restart cpa-manager-plus; exit 1; }
sed -i "s/$OLD/$PKG/g" $UNIT $B/run.sh
systemctl daemon-reload; systemctl restart cpa-manager-plus; sleep 4
systemctl is-active --quiet cpa-manager-plus || rollback
code=$(curl -s -o /dev/null -w '%{http_code}' -m 8 http://127.0.0.1:18317/) ; echo "local_http=$code"
[[ "$code" =~ ^(200|301|302|307|401)$ ]] || rollback
journalctl -u cpa-manager-plus -n 8 --no-pager | tail -8
echo "OK $CUR -> $TAG; data backup $B/backups/data.$TS.$CUR"
echo "ROLLBACK_CMD: cp -a $B/backups/cpa-manager-plus.service.$TS.$CUR $UNIT && cp -a $B/backups/run.sh.$TS.$CUR $B/run.sh && systemctl daemon-reload && systemctl restart cpa-manager-plus"
EOS
