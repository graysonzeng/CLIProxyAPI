# Weekly CLIProxyAPI / Management UI / CPA-Manager-Plus sync & deploy (Grok Bot box)

Runs entirely on the box; no Mac dependency. Target: `ssh ompcli` (root@43.165.66.65, key ~/.ssh/ompcli_deploy).

Repos (/workspace/cliproxy):
- `CLIProxyAPI` — origin=graysonzeng/CLIProxyAPI (fork), upstream=router-for-me/CLIProxyAPI. Integration branch: `workflow` (custom commits: 2880be3d model endpoint types, 3af6cf2f redisqueue usage fanout, ops scripts/docs).
- `mgmt` — router-for-me/Cli-Proxy-API-Management-Center, no custom commits: built from upstream main.
- CPA-Manager-Plus — official release tarballs only (seakee/CPA-Manager-Plus).

Steps (`./run.sh`):
1. Main-as-base: `sync` = upstream/main, merge origin/workflow (conflict -> stop, no deploy).
2. Gates: go build, go vet, go test vs upstream/main baseline (no new FAIL packages).
3. Push `sync:workflow` (fast-forward, never force). Push auth: git credential helper reading $OMP_GITHUB_TOKEN.
4. Deploy: `MGMT_ROOT=mgmt scripts/deploy-ompcli-3repo.sh` — config/OAuth fingerprint before/after, clean-worktree linux/amd64 build with Version/Commit/BuildDate ldflags + .meta/.sha256, backup + .meta, install, restart, health/exe-sha/journal checks, auto-rollback on failure; then management.html with backup + served-sha check.
5. CPAMP: `scripts/deploy-cpamp-ompcli.sh` — skip if current == latest release; else new runtime dir, copy config.json, backup unit/run.sh/data, retarget, daemon-reload, restart, rollback on failure. Never run install-cpamp.sh.
6. `./verify.sh` — public healthz, management.html 200, k.688663.xyz 307, /v1/models with key (key read on host only), deployed version.
Logs: progress.md. Rollback commands are printed by each deploy script.
