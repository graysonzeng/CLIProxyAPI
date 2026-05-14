# AGENTS.md

Go 1.26+ proxy server providing OpenAI/Gemini/Claude/Codex/Antigravity/Kiro compatible APIs with OAuth and round-robin load balancing.

## Repository
- GitHub: https://github.com/router-for-me/CLIProxyAPI

## Commands
```bash
gofmt -w . # Format (required after Go changes)
go build -o cli-proxy-api ./cmd/server # Build
go run ./cmd/server # Run dev server
go test ./... # Run all tests
go test -v -run TestName ./path/to/pkg # Run single test
go build -o test-output ./cmd/server && rm test-output # Verify compile (REQUIRED after changes)
```
- Common flags: `--config <path>`, `--tui`, `--standalone`, `--local-model`, `--no-browser`, `--oauth-callback-port <port>`
- After Go changes, run focused tests for touched packages first, then `go build -o test-output ./cmd/server && rm test-output`. Run `go test ./...` when shared provider routing, runtime, translator, API, auth, storage, config, or SDK behavior changes.
- For the server named `cli` in SSH config, do not restart services directly. Provide the exact restart command for the user to run instead. Other operations are allowed unless separately restricted.

## Config
- Default config: `config.yaml` (template: `config.example.yaml`)
- `.env` is auto-loaded from the working directory
- Auth material defaults under `auths/`
- Storage backends: file-based default; optional Postgres/git/object store (`PGSTORE_*`, `GITSTORE_*`, `OBJECTSTORE_*`)

## Architecture
- `cmd/server/` — Server entrypoint
- `cmd/fetch_antigravity_models/` — Antigravity model-fetch utility
- `internal/api/` — Gin HTTP API (routes, middleware, modules)
- `internal/api/modules/amp/` — Amp integration (Amp-style routes + reverse proxy)
- `internal/thinking/` — Main thinking/reasoning pipeline. `ApplyThinking()` (apply.go) parses suffixes (`suffix.go`, suffix overrides body), normalizes config to canonical `ThinkingConfig` (`types.go`), normalizes and validates centrally (`validate.go`/`convert.go`), then applies provider-specific output via `ProviderApplier`. Do not break this "canonical representation → per-provider translation" architecture.
- `internal/thinking/provider/` — Provider-specific thinking adapters
- `internal/runtime/executor/` — Per-provider runtime executors (incl. Codex WebSocket)
- `internal/runtime/executor/helps/` — Shared executor helpers
- `internal/translator/` — Provider protocol translators (and shared `common`)
- `internal/registry/` — Model registry + remote updater (`StartModelsUpdater`); `--local-model` disables remote updates
- `internal/store/` — Storage implementations and secret resolution
- `internal/managementasset/` — Config snapshots and management assets
- `internal/cache/` — Request signature caching
- `internal/watcher/` — Config hot-reload and watchers
- `internal/wsrelay/` — WebSocket relay sessions
- `internal/usage/` — Usage and token accounting
- `internal/tui/` — Bubbletea terminal UI (`--tui`, `--standalone`)
- `sdk/cliproxy/` — Embeddable SDK entry (service/builder/watchers/pipeline)
- `test/` — Cross-module integration tests

## Provider Development
- Trace provider changes end-to-end before editing: config shape, provider registration, auth material, secret resolution, model registry/mapping, translator, executor, streaming, tools, multimodal, thinking, usage accounting, and tests.
- Kiro or other provider-related features may reference these local open-source repositories as evidence and design references:
  - `/Users/sheng/tencent/AIClient2API`
  - `/Users/sheng/tencent/kiro.rs`
- For management API, Web UI, or frontend-related work, reference the management center repository:
  - `/Users/sheng/tencent/Cli-Proxy-API-Management-Center` — React + TypeScript + Vite single-page Web UI for the Management API (`/v0/management`). Handles config viewing/editing, credential uploads, and log viewing. Ships as a bundled single HTML file embedded in this server via `internal/managementasset/`.
- Treat reference repositories as inputs, not truth. Check their current source, compare against this repository's architecture, and respect license/attribution requirements before porting code or protocol details.
- Kiro-specific code currently lives in `internal/runtime/executor/kiro_executor.go`, `internal/runtime/executor/kiro_executor_test.go`, `internal/runtime/executor/helps/kiro_helpers.go`, and `internal/runtime/executor/helps/kiro_helpers_test.go`.

## Code Conventions
- Read before changing. Do not invent behavior, files, tests, or verification; if evidence is missing, state what must be checked.
- Keep changes small and simple (KISS)
- Comments in English only
- If editing code that already contains non-English comments, translate them to English (don’t add new non-English comments)
- For user-visible strings, keep the existing language used in that file/area
- New Markdown docs should be in English unless the file is explicitly language-specific (e.g. `README_CN.md`)
- As a rule, do not make standalone changes to `internal/translator/`. You may modify it only as part of broader changes elsewhere.
- If a task requires changing only `internal/translator/`, run `gh repo view --json viewerPermission -q .viewerPermission` to confirm you have `WRITE`, `MAINTAIN`, or `ADMIN`. If you do, you may proceed; otherwise, file a GitHub issue including the goal, rationale, and the intended implementation code, then stop further work.
- `internal/runtime/executor/` should contain executors and their unit tests only. Place any helper/supporting files under `internal/runtime/executor/helps/`.
- Follow `gofmt`; keep imports goimports-style; wrap errors with context where helpful
- Do not use `log.Fatal`/`log.Fatalf` (terminates the process); prefer returning errors and logging via logrus
- Shadowed variables: use method suffix (`errStart := server.Start()`)
- Wrap defer errors: `defer func() { if err := f.Close(); err != nil { log.Errorf(...) } }()`
- Use logrus structured logging; avoid leaking secrets/tokens in logs
- Avoid panics in HTTP handlers; prefer logged errors and meaningful HTTP status codes
- Timeouts are allowed only during credential acquisition; after an upstream connection is established, do not set timeouts for any subsequent network behavior. Intentional exceptions that must remain allowed are the Codex websocket liveness deadlines in `internal/runtime/executor/codex_websockets_executor.go`, the wsrelay session deadlines in `internal/wsrelay/session.go`, the management APICall timeout in `internal/api/handlers/management/api_tools.go`, and the `cmd/fetch_antigravity_models` utility timeouts
- Do not edit generated files directly. Find the schema, source definition, or generation command first.
- Treat OAuth credentials, API keys, session IDs, cookies, authorization headers, and account metadata as secrets. Redact them from logs, errors, tests, fixtures, and docs.

## Change Workflow
- Before changing code, identify the real entrypoint: route, CLI flag, config key, watcher, executor, translator, auth flow, storage path, or test.
- Before changing functions, fields, enums, config keys, routes, model names, cache keys, or provider names, search all producers and consumers.
- After changes, verify the original path and the relevant tests. Do not report tests or builds as passing unless they were actually run.
- If verification requires restarting a service on the SSH host named `cli`, stop before doing it and hand the restart command to the user.
