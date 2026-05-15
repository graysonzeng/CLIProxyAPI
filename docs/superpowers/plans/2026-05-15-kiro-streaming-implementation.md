# Implementation: Kiro Streaming Stability Governance

- Date: 2026-05-15
- Design input: `docs/kiro-streaming-design.md`
- Review input: `docs/superpowers/plans/2026-05-15-kiro-streaming-design-review.md`
- Status: Implemented and verified

## 1. Review Findings Handling

| Finding | Decision | Result |
| --- | --- | --- |
| HIGH-1 upstream timeout conflicts with `AGENTS.md:74` | Accepted | Removed the upstream timeout design. No `streaming.upstream-*` config or post-connect watchdog was added. |
| HIGH-2 conductor vs handler retry semantics are mixed | Accepted | Revised the design to make conductor `readStreamBootstrap` / `executeStreamWithModelPool` / `empty_stream` the primary bootstrap-failure path, with handler `bootstrap-retries` only as a secondary fallback. |
| HIGH-3 CountTokens approximate lacks Claude tokenizer and client compatibility evidence | Accepted | Kept Kiro `CountTokens` as HTTP 501 and added a project-prefixed unsupported marker. No approximate translator path was added. |
| MEDIUM-1 Kiro `empty_stream` overlaps conductor code | Accepted | Did not add Kiro-side `empty_stream`; added only streaming-specific Kiro error classes. |
| MEDIUM-2 timeout config namespace ambiguity | Accepted by deletion | No new timeout config was introduced. |
| MEDIUM-3 terminal error / conductor verification gaps | Partially implemented | Added focused Kiro executor tests for pre-payload empty/read/malformed errors, post-payload read error, failure usage, and CountTokens 501. Existing handler/conductor tests remain the broader coverage. |

## 2. Adopted Design Revisions

- `docs/kiro-streaming-design.md` was rewritten to include the missing root-cause evidence:
  - `sdk/cliproxy/auth/conductor.go:786`-`:816` for `readStreamBootstrap`.
  - `sdk/cliproxy/auth/conductor.go:818`-`:864` for `wrapStreamResult` and `MarkResult`.
  - `sdk/cliproxy/auth/conductor.go:936`-`:944` for conductor `empty_stream`.
  - `AGENTS.md:74` for the post-connection timeout rule.
  - `internal/runtime/executor/helps/token_helpers.go:11`-`:37` for the absence of Claude-native tokenizer support.
- The revised design now explicitly says:
  - Do not add upstream first-event/read timeout config.
  - Propagate errors into conductor first; handler bootstrap retry remains secondary.
  - Keep CountTokens 501 until tokenizer deviation and client compatibility evidence exist.

## 3. Implementation Summary

- `internal/runtime/executor/kiro_executor.go`
  - `CountTokens` still returns HTTP 501, now with `cpa_kiro_count_tokens_unsupported` in the error message.
  - `ExecuteStream` now captures `streamKiroToClaudeSSE` errors, publishes failure usage, and sends `StreamChunk{Err: ...}` downstream.
  - `reporter.EnsurePublished(ctx)` is only called after the converter successfully sends payload.
  - `streamKiroToClaudeSSE` now returns `kiroStreamResult, error`.
  - `message_start` is emitted only after the first valid Kiro event.
  - Empty stream / pre-event EOF emits no payload and returns nil so conductor can use its existing `empty_stream` path.
  - Non-EOF read errors and malformed partial JSON return Kiro streaming errors and do not emit success terminal events.
- `internal/runtime/executor/helps/kiro_helpers.go`
  - Added `KiroErrStreamRead` and `KiroErrStreamMalformed` classes.
  - Did not add Kiro-side `empty_stream`.
- `internal/runtime/executor/kiro_executor_test.go`
  - Added coverage for empty stream, pre-payload read error, pre-payload malformed JSON, post-payload read error without `message_stop`, failure usage, and CountTokens 501 marker.

## 4. Verification Results

Commands run:

- `gofmt -w internal/runtime/executor/kiro_executor.go internal/runtime/executor/kiro_executor_test.go internal/runtime/executor/helps/kiro_helpers.go`
- `go test ./internal/runtime/executor -run 'TestStreamKiroToClaudeSSE|TestKiroExecutorExecuteStreamPublishes|TestKiroExecutorCountTokensKeepsUnsupported'` — PASS
- `go test ./internal/runtime/executor/helps ./internal/runtime/executor` — PASS
- `go build -o test-output ./cmd/server && rm test-output` — PASS
- `go test ./...` — PASS

## 5. Known Limits and Remaining Risk

- This pass does not implement a full AWS Event Stream binary frame decoder.
- This pass does not add upstream idle / first-event timeout, by design and per `AGENTS.md:74`.
- This pass does not implement CountTokens approximate; it remains blocked on tokenizer deviation baseline and client compatibility evidence.
- Cross-protocol terminal error tests rely mostly on existing handler/conductor coverage; a future follow-up can add Kiro-specific OpenAI/Gemini/Responses fake-upstream integration tests.
- The working tree contains unrelated pre-existing modifications outside this pass; this implementation intentionally changed only Kiro streaming design/executor/helper/test files.

## 6. Next Step Handoff

**Same-session continuation:**

```text
直接执行 $code-review 或 /code-review
```

**New-session recovery prompt:**

```text
请阅读设计输入 docs/kiro-streaming-design.md、
实现文档 docs/superpowers/plans/2026-05-15-kiro-streaming-implementation.md，
以及本次提交的代码变更，
使用 $code-review（或 /code-review）进行方案重审及代码审查。
```
