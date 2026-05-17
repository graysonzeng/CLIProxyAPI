# AGENTS.md

This repository is a Go 1.26+ proxy server that exposes OpenAI, Gemini, Claude,
Codex, Antigravity, Kiro, and other provider-compatible APIs for CLI clients. It
supports OAuth-backed accounts, API-key providers, streaming, non-streaming,
WebSocket flows, round-robin account balancing, management APIs, and an
embeddable SDK.

## Core Rule

Read before changing. Do not invent behavior, files, tests, or verification.
Every non-trivial conclusion should come from code, config, docs, or command
output in this repository. If evidence is missing, say what must be checked next.

## Commands

```bash
gofmt -w .                                      # Format after Go changes
go build -o cli-proxy-api ./cmd/server          # Build server
go run ./cmd/server                             # Run dev server
go test ./...                                   # Run all tests
go test -v -run TestName ./path/to/pkg          # Run focused tests
go build -o test-output ./cmd/server && rm test-output
```

The final compile check above is required after Go changes. Run focused tests
for the touched packages first, then broaden when shared provider, translator,
runtime, API, auth, or storage behavior is affected.

For the server named `cli` in SSH config, do not restart services directly.
Provide the exact restart command for the user to run instead. Other operations
are allowed unless separately restricted.

Common runtime flags:

```bash
--config <path>
--tui
--standalone
--local-model
--no-browser
--oauth-callback-port <port>
```

## Configuration

- Default config file: `config.yaml`
- Template config: `config.example.yaml`
- `.env` is auto-loaded from the working directory.
- Auth material defaults under `auths/`.
- Storage defaults to file-backed storage.
- Optional storage backends use `PGSTORE_*`, `GITSTORE_*`, and `OBJECTSTORE_*`
  environment variables.
- `--local-model` disables remote model registry updates.

## Architecture Map

- `cmd/server/`: server entrypoint.
- `cmd/fetch_antigravity_models/`: utility for fetching Antigravity models.
- `internal/api/`: Gin HTTP API, routes, middleware, and modules.
- `internal/api/modules/amp/`: Amp routes, model mapping, fallback handlers,
  response rewriting, and reverse proxy support.
- `internal/auth/`: provider authentication implementations.
- `internal/config/`: config structures and loading behavior.
- `internal/thinking/`: canonical thinking/reasoning pipeline.
- `internal/thinking/provider/`: provider-specific thinking adapters.
- `internal/runtime/executor/`: provider runtime executors and executor tests.
- `internal/runtime/executor/helps/`: executor support helpers.
- `internal/translator/`: provider protocol translators.
- `internal/registry/`: model registry and remote updater.
- `internal/store/`: storage implementations and secret resolution.
- `internal/managementasset/`: config snapshots and management assets.
- `internal/cache/`: request signature caching.
- `internal/watcher/`: config hot reload and watcher support.
- `internal/wsrelay/`: WebSocket relay sessions.
- `internal/usage/`: usage and token accounting.
- `internal/tui/`: Bubbletea terminal UI for `--tui` and `--standalone`.
- `sdk/cliproxy/`: embeddable SDK service, builder, watchers, and pipeline.
- `test/`: cross-module integration tests.

## Thinking Pipeline

Preserve the canonical thinking architecture:

1. `internal/thinking/ApplyThinking()` parses suffix overrides.
2. The request is normalized to canonical `ThinkingConfig`.
3. Central validation and conversion run through `validate.go` and `convert.go`.
4. Provider-specific output is applied through `ProviderApplier`.

Do not bypass this by adding provider-only thinking logic directly inside
executors or translators unless the existing architecture already requires that
edge behavior there.

## Provider Development

Provider work usually touches several layers. Trace the full path before
editing:

1. Config shape and provider registration.
2. Auth material and secret resolution.
3. Model registry or model mapping behavior.
4. Request translation.
5. Runtime executor behavior.
6. Streaming, tool calling, multimodal, thinking, and usage accounting.
7. Tests and fixtures for the affected provider path.

For Kiro or other provider-related features, it is acceptable and encouraged to
reference these local open-source repositories:

- `/Users/sheng/tencent/AIClient2API`
- `/Users/sheng/tencent/kiro.rs`

For management API, Web UI, or frontend-related work, reference the management
center repository:

- `/Users/sheng/tencent/Cli-Proxy-API-Management-Center` — React + TypeScript +
  Vite single-page Web UI for the Management API (`/v0/management`). Handles
  config viewing/editing, credential uploads, and log viewing. Ships as a bundled
  single HTML file embedded in this server via `internal/managementasset/`.

Use them as evidence and design references, not as unquestioned truth. Check
their current source, compare behavior against this repository's architecture,
and respect license and attribution requirements before porting code or protocol
details.

Kiro-specific code currently lives in:

- `internal/runtime/executor/kiro_executor.go`
- `internal/runtime/executor/kiro_executor_test.go`
- `internal/runtime/executor/helps/kiro_helpers.go`
- `internal/runtime/executor/helps/kiro_helpers_test.go`

### Claude Code over Kiro Long-Task Recovery

When Claude Code over Kiro stalls or fails during long FlowDeck-style tasks,
debug the full streaming chain before changing behavior:

1. Claude `/v1/messages` streaming handler.
2. `ExecuteStreamWithAuthManager` stream bootstrap and first-payload behavior.
3. Auth conductor retry, retry-after, and cooldown handling.
4. Kiro executor HTTP status classification, especially Cloudflare 524
   `origin_response_timeout`.
5. SSE keep-alive behavior before the first model payload.
6. FlowDeck artifact contract symptoms and terminal logs.

Minimum regression coverage for this class of fix:

- Handler emits SSE keep-alive before the first model payload when configured.
- Stream bootstrap returns channels without blocking on the first payload.
- Kiro/Cloudflare 524 exposes `retry_after` from either headers or JSON body.
- Conductor retries 524 with retry-after when retry config allows it.

Validation flow used for the 2026-05 Claude/Kiro 524 repair:

1. Run focused tests for `sdk/api/handlers`,
   `sdk/api/handlers/claude`, `internal/runtime/executor/helps`,
   `internal/runtime/executor`, and `sdk/cliproxy/auth`.
2. Run `go test ./...`.
3. Run `go build -o test-output ./cmd/server && rm test-output`.
4. Repeat the focused package test plus build loop three times for release
   confidence.
5. For `ssh cli` deployment, build `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`,
   upload to `/root/cliproxyapi/cli-proxy-api.deploy.tmp`, verify sha256 and
   `-h` version output, back up the existing binary, then atomically replace
   `/root/cliproxyapi/cli-proxy-api`.
6. Do not restart `cliproxyapi.service` directly from an agent session. Provide
   the exact restart command to the user.
7. After the user restarts, verify `systemctl is-active`, process binary hash,
   `/healthz` three times on the configured port, and at least three real Kiro
   streaming `/v1/messages` smoke requests that reconstruct the expected text.
8. Check recent service logs for `524`, `origin_response_timeout`, `panic`,
   `fatal`, and startup/listen errors. Warmup warnings alone are not a failure
   if real Kiro streaming requests pass.

Do not claim FlowDeck/Claude CLI recovery unless a local or real FlowDeck
pipeline or smoke was actually run.

## Translator Guardrail

Do not make standalone changes only under `internal/translator/`.

If a task truly requires changing only `internal/translator/`, first run:

```bash
gh repo view --json viewerPermission -q .viewerPermission
```

Proceed only with `WRITE`, `MAINTAIN`, or `ADMIN`. Otherwise, open a GitHub
issue with the goal, rationale, and intended implementation, then stop.

## Executor Guardrails

- Keep executor code and executor unit tests under `internal/runtime/executor/`.
- Put shared executor helper/support code under `internal/runtime/executor/helps/`.
- Avoid leaking tokens, account IDs, authorization headers, cookies, or secrets
  in logs.
- Use structured logrus logging.
- Do not use `log.Fatal` or `log.Fatalf`; return errors and let callers decide.
- Avoid panics in HTTP handlers.
- Wrap errors with context when it helps the caller or log reader.

## Timeout Rule

Timeouts are allowed only during credential acquisition. After an upstream
connection is established, do not add timeouts for later network behavior.

Intentional exceptions that may keep deadlines or timeouts:

- Codex WebSocket liveness deadlines in
  `internal/runtime/executor/codex_websockets_executor.go`
- wsrelay session deadlines in `internal/wsrelay/session.go`
- management API call timeout in `internal/api/handlers/management/api_tools.go`
- `cmd/fetch_antigravity_models` utility timeouts

## Code Style

- Keep changes small and simple.
- Follow existing package boundaries and naming.
- Run `gofmt -w .` after Go edits.
- Keep imports goimports-style.
- Comments must be in English.
- If editing code that already has non-English comments, translate those comments
  to English instead of adding more non-English comments.
- User-visible strings should follow the existing language in that file or area.
- New Markdown docs should be in English unless the file is explicitly
  language-specific, such as `README_CN.md`.
- Use method-specific error variable names when shadowing would be confusing,
  such as `errStart := server.Start()`.
- Wrap deferred close errors:

```go
defer func() {
	if err := f.Close(); err != nil {
		log.Errorf("close file: %v", err)
	}
}()
```

## Change Workflow

Before changing code:

1. Identify the real entrypoint: route, CLI flag, config key, watcher, executor,
   translator, auth flow, storage path, or test.
2. Search all producers and consumers for changed functions, fields, enums,
   config keys, routes, model names, cache keys, and provider names.
3. Check tests near the touched package and related integration tests.
4. State any unknowns when evidence is incomplete.

After changing code:

1. Run focused tests for touched packages.
2. Run `gofmt -w .` for Go changes.
3. Run `go build -o test-output ./cmd/server && rm test-output`.
4. Run broader `go test ./...` when provider routing, shared runtime,
   translators, API middleware, storage, config, or SDK behavior changes.
5. If verification requires restarting a service on the SSH host named `cli`,
   stop before doing it and hand the restart command to the user.

## Generated Code

Do not edit generated files directly. Find the schema, source definition, or
generation command first, then update the source and regenerate.

## Security

- Treat OAuth credentials, API keys, session IDs, cookies, and account metadata
  as secrets.
- Redact secrets from logs, errors, tests, fixtures, and docs.
- Be careful with provider headers, management endpoints, localhost-only routes,
  CORS behavior, proxy forwarding, and request cloaking.
- For management or persistence changes, verify read/write scope and rollback
  behavior before writing data.
