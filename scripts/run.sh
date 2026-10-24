#!/usr/bin/env bash
# Weekly CLIProxyAPI + Management UI + CPA-Manager-Plus sync/build/deploy/verify (runs on Grok Bot box).
# Usage: ./run.sh [--no-deploy]
set -euo pipefail
W=/workspace/cliproxy; R=$W/CLIProxyAPI; M=$W/mgmt; LOG=$W/progress.md
log(){ echo "- $(date '+%F %T') $*" | tee -a $LOG; }
cd $R; git fetch -q upstream; git fetch -q origin
git checkout -q -B sync upstream/main
UP=$(git rev-parse --short upstream/main)
if ! git merge --no-edit origin/workflow >/dev/null; then log "CONFLICT merging workflow onto upstream $UP; resolve manually"; git diff --name-only --diff-filter=U | tee -a $LOG; exit 2; fi
go build ./... && go vet ./... || { log "build/vet failed"; exit 3; }
rm -rf $W/base; git worktree prune; git worktree add -q $W/base upstream/main
(cd $W/base && go test ./... 2>&1 | grep -E '^FAIL\s' | sort > $W/base-fail.txt || true)
go test ./... 2>&1 | grep -E '^FAIL\s' | sort > $W/merged-fail.txt || true
NEW=$(comm -13 $W/base-fail.txt $W/merged-fail.txt)
[[ -z "$NEW" ]] || { log "new test failures: $NEW"; exit 4; }
git push -q origin sync:workflow   # fast-forward only (sync contains origin/workflow)
log "pushed workflow=$(git rev-parse --short HEAD) upstream=$UP"
git checkout -q --detach origin/workflow 2>/dev/null; git fetch -q origin; git checkout -q --detach origin/workflow
cd $M; git fetch -q origin; git checkout -q --detach origin/main; git clean -fdq -e node_modules
[[ "${1:-}" == "--no-deploy" ]] && exit 0
cd $R
MGMT_ROOT=$M SSH_HOST=ompcli ./scripts/deploy-ompcli-3repo.sh || { log "deploy API/UI failed (auto-rollback attempted)"; exit 5; }
./scripts/deploy-cpamp-ompcli.sh || { log "CPAMP upgrade failed (rolled back)"; exit 6; }
$W/verify.sh | tee -a $LOG; [[ ${PIPESTATUS[0]} == 0 ]] || { log "MANDATORY chat verification failed -> auto-rollback API binary + UI"; ssh ompcli 'B=$(ls -t /root/cliproxyapi/cli-proxy-api.backup.*[0-9a-f] | head -1); H=$(ls -t /root/cliproxyapi/static/management.html.backup.*[0-9a-f] | head -1); cp -a $B /root/cliproxyapi/cli-proxy-api && cp -a $H /root/cliproxyapi/static/management.html && systemctl restart cliproxyapi.service && echo restored $B $H' | tee -a $LOG; exit 7; }
