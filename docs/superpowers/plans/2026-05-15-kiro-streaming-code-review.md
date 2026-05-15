# Code Review: Kiro Streaming Stability Governance

- Date: 2026-05-15
- Design input: `docs/kiro-streaming-design.md`
- Implementation document: `docs/superpowers/plans/2026-05-15-kiro-streaming-implementation.md`
- Reviewer scope: working-tree changes to Kiro streaming life-cycle, error
  classes, executor tests, and the supporting design/registry artefacts.
- Conclusion: **PASS_WITH_NOTES**

## 1. 审查范围

| 文件 | 改动性质 | 是否在设计 / 实现文档声明范围 |
| --- | --- | --- |
| `internal/runtime/executor/kiro_executor.go` | streaming 生命周期重写 + CountTokens 标记 | 是 |
| `internal/runtime/executor/kiro_executor_test.go` | 新增空流 / 读错 / malformed / 失败 usage 测试 | 是 |
| `internal/runtime/executor/helps/kiro_helpers.go` | 新增 `KiroErrStreamRead`/`KiroErrStreamMalformed`；同时新增 `haiku`、`opus 4.7`、`opus-4.7`、`claude-haiku-4-5-20251001` 模型映射 | streaming error class **是**；模型映射 **不是**（impl 文档未声明） |
| `internal/runtime/executor/helps/kiro_helpers_test.go` | 跟随 streaming class + 新增模型映射测试 | streaming **是**；模型 **不是** |
| `docs/kiro-streaming-design.md` | 设计文档重写 | 是 |
| `internal/registry/models/models.json` | 新增 `claude-haiku-4-5-20251001`、`haiku`、`opus 4.7`、`opus-4.7` 四个 Kiro alias 条目 | **不是**（impl 文档声称仅改 streaming 设计/executor/helper/test） |

工作树中其余被修改的文件（`internal/api/handlers/management/*`、`sdk/cliproxy/auth/codex_queue*`、`internal/config/*`、`internal/api/server.go`、`config.example.yaml`、`internal/api/handlers/management/warmup*`、未跟踪的截图与二进制等）属于 codex queue / management / config 其他工作流，本次设计文档与实现文档明确不在 Kiro streaming 范围之内，本审查不评估其逻辑正确性，仅在 §5 给出 scope 提示。

执行的本地校验：

- `gofmt -l internal/runtime/executor/kiro_executor.go internal/runtime/executor/kiro_executor_test.go internal/runtime/executor/helps/kiro_helpers.go internal/runtime/executor/helps/kiro_helpers_test.go`：无输出。
- `go test ./internal/runtime/executor/helps -count=1`：PASS。
- `go test ./internal/runtime/executor -count=1`：PASS。
- `go test ./internal/runtime/executor -run 'TestStreamKiroToClaudeSSE|TestKiroExecutorExecuteStreamPublishes|TestKiroExecutorCountTokensKeepsUnsupported' -count=1`：PASS。
- `go build -o test-output ./cmd/server && rm test-output`：BUILD_OK。
- `go test ./...`：PASS（含 `sdk/cliproxy/auth`、`sdk/api/handlers`、`test/` 等）。

## 2. 设计一致性评估

| 设计点 | 实现位置 | 结论 |
| --- | --- | --- |
| `streamKiroToClaudeSSE` 改为返回 `kiroStreamResult, error`，记录 `eventCount` / `payloadStarted` | `internal/runtime/executor/kiro_executor.go:1160`-`:1163`、`:1169` | 一致 |
| `message_start` 延迟到第一个有效 Kiro event 之后 | `internal/runtime/executor/kiro_executor.go:1193`-`:1209`、`:1366`-`:1372` | 一致（`processEvents` 内每个事件先 `emitMessageStart()` 再 `processEvent`） |
| 正常 EOF 且 `eventCount=0/payloadStarted=false` 不发 payload、不发 success usage | `internal/runtime/executor/kiro_executor.go:1409`-`:1411`、`:286`-`:320` | 一致；`reporter.EnsurePublished` 仅在 `payloadStarted` 时调用 |
| 首事件前 read error / malformed → 返回 KiroError | `internal/runtime/executor/kiro_executor.go:1389`-`:1407`、`:1450`-`:1455` | 一致（class 落在 `KiroErrStreamRead` / `KiroErrStreamMalformed`） |
| 首事件前 ctx cancel → 返回 ctx.Err()（设计允许此选项） | `internal/runtime/executor/kiro_executor.go:1378`-`:1395` | 一致；选了 `ctx.Err()` 而非 `KiroErrStreamCancelled`，设计两选一允许 |
| 已发送 payload 后失败时不再补 `message_stop` | `internal/runtime/executor/kiro_executor.go:1409`-`:1411` 之后才发 stop | 一致；测试 `TestStreamKiroToClaudeSSE_ReadErrorAfterPayloadDoesNotEmitStop` 验证 |
| 错误通过 `StreamChunk{Err}` 上抛，使 conductor `readStreamBootstrap` 截获 | `internal/runtime/executor/kiro_executor.go:310`-`:316` | 一致；与 `sdk/cliproxy/auth/conductor.go:786`-`:816` 的语义对齐 |
| 失败时使用 `reporter.PublishFailure`，成功才 `EnsurePublished` | `internal/runtime/executor/kiro_executor.go:310`-`:320` | 一致；`UsageReporter.once.Do` 保证唯一发布（`internal/runtime/executor/helps/usage_helpers.go:99`、`:128`） |
| `CountTokens` 保持 501，并增加 `cpa_kiro_count_tokens_unsupported` 标识 | `internal/runtime/executor/kiro_executor.go:67`-`:69` | 一致 |
| 不引入任何 `streaming.upstream-*` / 等价 watchdog 配置 | `internal/config/sdk_config.go:49`-`:59` 未变；本次工作树也未新增 | 一致，遵守 `AGENTS.md:74` |
| 不新增 Kiro-side `empty_stream` 错误码 | `helps/kiro_helpers.go:496`-`:530` | 一致；首事件前 EOF 让 conductor 走 `sdk/cliproxy/auth/conductor.go:936`-`:944` 的 `empty_stream` |

设计 §6 验证计划与实际测试覆盖对照：

| 设计要求 | 状态 |
| --- | --- |
| Unit：正常 text/tool/thinking 顺序 + 延迟 message_start | 已覆盖（既有用例 + `TestStreamKiroToClaudeSSE_Incremental` 已更新） |
| Unit：首事件前 EOF / 空流不 emit payload，不发 success usage | 已覆盖（`TestStreamKiroToClaudeSSE_EmptyStreamLeavesConductorEmptyStreamPath` + `TestKiroExecutorExecuteStreamPublishesFailureOnMalformedStream` 间接证明 success 不发） |
| Unit：首事件前 read error / malformed | 已覆盖 |
| Unit：首事件前 ctx cancel | **未覆盖** |
| Unit：首事件后 read error 不发 message_stop | 已覆盖 |
| Unit：首事件后 ctx cancel / malformed | **未覆盖** |
| Integration：fake upstream + conductor empty_stream + model-pool failover | 部分覆盖（仅 executor → channel 层级，没有走 conductor `executeStreamWithModelPool`） |
| Integration：已发送 payload 后断流，handler 输出 terminal error | **未直接覆盖** |
| Integration：跨协议 terminal error（OpenAI / Gemini / Responses） | **未覆盖**，impl 文档已自承 |
| Integration：`wrapStreamResult` `MarkResult(Success:false)` | **未直接覆盖** |
| CountTokens 501 | 已覆盖 |
| Build / test 矩阵 | 已覆盖 |

## 3. 主要发现

### [HIGH] scope: 实现文档与实际改动范围不一致

**文件**：`internal/runtime/executor/helps/kiro_helpers.go:55`-`:70`、`internal/runtime/executor/helps/kiro_helpers_test.go:104`-`:113`、`internal/registry/models/models.json:2107`-`:2230`、`docs/superpowers/plans/2026-05-15-kiro-streaming-implementation.md:64`

**问题**：实现文档第 5 节明确写：「intentionally changed only Kiro streaming design/executor/helper/test files」，但工作树中 `kiro_helpers.go` 同时新增了与 streaming 完全无关的模型 alias（`haiku`、`opus 4.7`、`opus-4.7`、`claude-haiku-4-5-20251001`），并且 `internal/registry/models/models.json` 也添加了这四个 Kiro 模型条目。这两个改动既不在设计文档 §1.3 / §5 范围内，也未在实现文档「Implementation Summary」中提及。

**影响**：
- 把"streaming 稳定性"和"模型 alias 扩充"两个独立工作捆在同一次未提交改动里，code-review 边界混乱，回滚 / cherry-pick 时极易把不相关风险拉进来。
- 实现文档自述与实际改动不符，读者无法仅凭文档判断真实变更面，违背 `CLAUDE.md` 的「Read before changing. Do not invent behavior, files, tests, or verification」精神。
- 模型 alias 中 `"opus 4.7"`（含空格）作为 client-facing model id，会跟其它 provider 的命名习惯偏离，潜在与 URL/Header 解析或 routing key 产生兼容隐患（虽然走的是直接 map lookup，但 model registry 里同时落地，会被列在公开 `/v1/models` 输出）。

**建议**：
1. 修复阶段把模型 alias 从本次 streaming PR 拆分出去，单独成一个明确的 "Kiro model alias" 改动并补独立设计/审查记录。
2. 或者保留同一次提交，但更新 `2026-05-15-kiro-streaming-implementation.md` §3、§5 与 `docs/kiro-streaming-design.md` §1.3，明确把 `helps.KiroModelMapping` 与 `models.json` 列入范围，并对带空格的 `"opus 4.7"` alias 做一次必要性 / 替代项评估（建议至少保留 `opus-4.7`，删除 `"opus 4.7"`）。
3. 不论哪种处理，都需要补 `MapKiroModel` 在大小写 / 前后空格上的归一化测试（当前测试只覆盖逐字 alias）。

### [MEDIUM] code: `buildClaudeMessageSSE` 在非流式 Execute 路径上行为静默退化

**文件**：`internal/runtime/executor/kiro_executor.go:1135`-`:1144`、`:230`-`:242`、`:1409`-`:1411`

**问题**：`streamKiroToClaudeSSE` 现在只有 `payloadStarted=true` 时才补 `message_delta` / `message_stop`。`buildClaudeMessageSSE` 也复用同一函数（传入 `bytes.NewReader(rawResp)`、`context.Background()` 并丢弃返回的 `(result, error)`）。这意味着：

- 当 Kiro 非流式 200 响应体为空、或残留尾部 partial JSON 时，`buildClaudeMessageSSE` 现在返回**空 SSE buffer**；旧实现固定会输出 `message_start + message_delta + message_stop`。
- `Execute(...)` 对 cross-protocol（`from != to`）路径会把这个空 SSE 喂给 `sdktranslator.TranslateNonStream`，最终客户端可能拿到译者基于"空输入"产生的退化产物（视各 translator 实现而定），且执行器层完全没有 error 信号，因为 `_, _ = streamKiroToClaudeSSE(...)` 把错误吞了。

**影响**：
- 设计/实现文档只声明 streaming lifecycle，没说会改非流式跨协议输出；这是隐性副作用。
- 单测目前只覆盖了正常 rawResp 情况（`TestBuildClaudeMessageSSE_TranslatesToOpenAINonStream`），缺少空 / malformed rawResp 的回归保护。
- 真实场景虽不常见，但 200 + 空 body 或被代理截断的非流式响应不能假定不发生，否则跨协议客户端会收到一个看似成功、实际为空的 OpenAI/Gemini 响应，运维侧很难发现。

**建议**：
1. 在 `buildClaudeMessageSSE` 内部捕获 `streamKiroToClaudeSSE` 的 error / `payloadStarted=false` 情况，补一个等价于"empty stream"的 fallback：要么显式生成最小 `message_start + message_delta + message_stop` 兼容序列，要么在外层 `Execute` 直接走失败路径（与 streaming 路径的 `PublishFailure` 对齐）。
2. 增加 `TestBuildClaudeMessageSSE_EmptyOrMalformed` 测试，断言 cross-protocol 调用在空/malformed Kiro response 下的可观察行为（最好是返回明确错误而不是静默退化）。
3. 顺手修一下 `_, _ = streamKiroToClaudeSSE(...)`：要么把 error log 出来（Warnf 即可，注意不要带 body），要么用 `result, err := ...; if err != nil { ... }` 结构以避免未来又被悄悄忽略。

### [MEDIUM] tests: 设计 §6 中 ctx-cancel / 跨协议 / conductor 集成验证未落地

**文件**：`internal/runtime/executor/kiro_executor_test.go:390`-`:665`、`docs/kiro-streaming-design.md:159`-`:169`

**问题**：设计 §6「验证计划」与实现文档 §1 都把 ctx cancel、跨协议 terminal error、conductor `empty_stream`/`MarkResult(Success:false)` 列为目标；实际新增测试只覆盖：

- 首事件前 EOF / 空流；
- 首事件前 read error；
- 首事件前 malformed JSON；
- 首事件后 read error 不补 stop；
- ExecuteStream malformed → failure usage；
- CountTokens 501 marker。

未覆盖：
- 任何 ctx 取消（pre-/post-payload）；
- conductor 层 `executeStreamWithModelPool` failover 行为；
- `wrapStreamResult` 在 chunk.Err 后 `MarkResult(Success:false)`；
- 跨协议 `from != to` 下的 streaming terminal error（OpenAI、Gemini、Responses）。

**影响**：核心设计目标"客户端取消 / 跨协议错误"在 CI 上没有断言保护，回归无法被自动发现；ctx 取消尤其值得担心，因为代码里两次 `contextErr(ctx)` 检查与 `for { ... }` 循环交互，没有测试很难证明无竞态。

**建议**：
1. 在 `internal/runtime/executor` 增加：
   - `TestStreamKiroToClaudeSSE_CtxCancelBeforePayloadReturnsCtxErr`：用 `context.WithCancel` 在第一次 `Read` 之前取消，断言 result.payloadStarted=false 且返回 `context.Canceled`。
   - `TestStreamKiroToClaudeSSE_CtxCancelAfterPayloadDoesNotEmitStop`：先喂一个 chunk 让 payload 起来，再触发取消，断言不发 message_stop。
2. 在 `sdk/cliproxy/auth` 或 `internal/runtime/executor` 加一个 conductor-level 集成用例：用 fake executor / fake upstream，让 Kiro 流首事件前以 `KiroErrStreamRead` 关闭，断言 `executeStreamWithModelPool` 在多 model pool 下做了 failover、并最终命中 `empty_stream` 或 `bootstrap_error`。
3. 至少用一个最小用例覆盖 cross-protocol（`source=openai`/`gemini`）情况下，发完 payload 再断流时 `wrapStreamResult` 会发 chunk.Err；可以基于现有 `TestKiroExecutorExecuteStreamPublishesFailureOnMalformedStream` 的 fake upstream 改造。

### [LOW] code: `hasKiroJSONResidue` 用 `strings.Contains` 而非"以 `{` 开头"判定

**文件**：`internal/runtime/executor/kiro_executor.go:1457`-`:1459`、`internal/runtime/executor/helps/kiro_helpers.go:275`-`:351`

**问题**：设计 §5.5 把 malformed 判据写为「EOF 后最终 parse 仍存在以 `{` 开头的 incomplete JSON」。当前实现只判 `strings.Contains(remaining, "{")`。`ParseAwsEventStreamBuffer` 现行实现保证 `remaining` 要么以 `{` 开头（incomplete JSON 分支），要么不含 `{`（searchStart 单步推进 + 只有走到 `strings.Index ... < 0` 才退出循环），所以两种写法语义等价。但这个等价性是依赖 parser 内部不变量的隐式约束。

**影响**：未来若有人调整 parser 的 search 推进逻辑（比如在 invalid JSON 时按 byte 跳更多步），`Contains` 的写法可能误判，把"junk + 半个新事件"或"junk 包含独立 `{`"判成 malformed，触发不必要的 conductor 失败。

**建议**：把 residue 判据写成 `strings.HasPrefix(strings.TrimLeft(remaining, " \t\r\n"), "{")`，明确呼应设计文本；并在 helper 测试中加一个 "尾部 garbage 但不含 `{`" 的用例与一个 "尾部 `{...` 不完整" 的用例，把不变量钉死。

### [LOW] code: `newKiroStreamError` 没有保留 cause，errors.Is/As 无法穿透

**文件**：`internal/runtime/executor/kiro_executor.go:1450`-`:1455`、`internal/runtime/executor/helps/kiro_helpers.go:541`-`:598`

**问题**：`newKiroStreamError` 只把 cause 拼到 Body，没有写 `KiroError.cause`。但 `KiroError.Unwrap()` 返回的是 `cause`。目前调用方没有 `errors.Is(streamErr, originalReadErr)` 的需求，所以不影响当前路径，但会让未来排查"是不是 net.OpError / net.ErrClosed"之类需求被迫去 parse Body 字符串。

**影响**：诊断 / 重试策略扩展性。

**建议**：把 `newKiroStreamError` 改成同时填 `Body` 与 `cause`（cause 用 raw error），并在测试里用 `errors.Is(err, readErr)` 断言。

### [LOW] log: `log.Warnf("kiro executor: error reading stream: %v", readErr)` 重复 + 可能含 IP

**文件**：`internal/runtime/executor/kiro_executor.go:1396`-`:1397`

**问题**：现有代码在判定 `KiroErrStreamRead` 之前调了 `log.Warnf("kiro executor: error reading stream: %v", readErr)`；同时又把 `readErr` 包进 KiroError 通过 `chunk.Err` 上抛，conductor 还会再 `MarkResult` log 一次。同一次 read error 会出现在执行器 warn、conductor mark、handler terminal writer 三处。`readErr` 通常是 `net.OpError`，会带 src/dst IP:port，长期日志可能积累为对端可观测信息。

**影响**：噪声 + 轻微的网络元数据冗余。

**建议**：
- 要么删掉这条 `log.Warnf`，让 conductor 端统一记录；
- 要么把它降为 `log.Debugf`，并在格式上去掉 `%v` 改用 `helps.ClassifyKiroNetworkError(readErr)` 这样的 class 标签，避免直接打印 net.OpError 文本。

### [LOW] cohesion: streaming-only 错误类与现有 `ClassifyKiroNetworkError` 关系不清

**文件**：`internal/runtime/executor/helps/kiro_helpers.go:496`-`:530`、`:643`-`:677`

**问题**：新增的 `KiroErrStreamRead` / `KiroErrStreamMalformed` 是 streaming-only 错误类，但 `IsTransientNetworkError` 没有把它们纳入考虑（仅识别 `KiroErrNetwork` / `KiroErrServer` / `KiroErrRateLimited`）。conductor 的 failover 行为目前依赖 `isRequestInvalidError` + `idx < len(execModels)-1` 判定，并不依赖 `IsTransientNetworkError`，所以现状不影响主流程；但如果以后有人在某个地方用 `IsTransientNetworkError` 决定是否换 credential，就会漏掉 stream_read。

**影响**：低；隐性的语义不一致。

**建议**：在 `IsTransientNetworkError` 中显式补 `KiroErrStreamRead`（malformed 不是 transient，可以排除），并加一个 helps 包级测试断言。

## 4. 改进建议汇总

1. **拆 PR / 修文档**：模型 alias（HIGH）从本次 streaming 范围拆走，或者把它正式纳入设计与实现文档。带空格的 `"opus 4.7"` alias 至少要论证必要性。
2. **补回 cross-protocol fallback**：`buildClaudeMessageSSE` 不能默默吞掉 streamErr / 空输出（MEDIUM），并补 `Execute` 路径的回归。
3. **补关键测试**：ctx cancel（pre/post payload）、conductor `executeStreamWithModelPool` failover、`wrapStreamResult` `MarkResult(Success:false)`、跨协议 streaming terminal error（MEDIUM）。
4. **小修小补**：`hasKiroJSONResidue` 用 `HasPrefix` + 空白 trim；`newKiroStreamError` 保留 `cause`；剔除或下调 read-error 重复 warn 日志；将 stream_read 纳入 `IsTransientNetworkError`（LOW）。

## 5. 工作树外 scope 提示（不影响本审查结论）

工作树同时含 `internal/api/handlers/management/api_tools.go`、`internal/api/server.go`、`internal/api/handlers/management/codex_queue.go`、`sdk/cliproxy/auth/codex_queue*.go`、`sdk/cliproxy/auth/conductor.go`、`internal/config/*.go`、`config.example.yaml`、`internal/api/handlers/management/warmup*.go`、未跟踪的 `cli-management-login.png`、`local-config-no-codex-queue.png`、`cli-proxy-api.linux-amd64`、`.playwright-mcp/*` 等改动。这些属于其他工作流（codex queue、management warmup 等），与本次 Kiro streaming 设计与实现文档无关；本审查不评估其逻辑、安全或测试覆盖度。建议在提交本次 Kiro streaming 改动时，将这部分变更与 Kiro streaming 改动**分两个独立提交**或两个独立 PR，避免审查与回滚责任混淆。

## 6. 最终结论

`PASS_WITH_NOTES`：streaming lifecycle、错误传播、failure usage 发布、CountTokens 标识与设计文档高度一致；`gofmt`、focused tests、`go test ./...`、`go build ./cmd/server` 全部通过。

但存在以下需要修复或澄清的问题，建议进入修复阶段处理：
- **HIGH**：scope/文档不一致——模型 alias 没在设计/实现文档列入范围，且 `"opus 4.7"` alias 需要重新评估。
- **MEDIUM**：非流式跨协议路径 `buildClaudeMessageSSE` 在空/malformed 响应下静默退化；缺 ctx cancel 与 conductor/cross-protocol 集成测试。
- **LOW**：`hasKiroJSONResidue` 等价但偏离设计文本、`newKiroStreamError` 不保留 cause、read-error 重复日志、`IsTransientNetworkError` 未涵盖新错误类。

## 7. Handoff

### 7.1 同会话继续

```
直接执行 $fix-implement 或 /fix-implement
```

### 7.2 新会话恢复 prompt

```
请阅读实现文档 docs/superpowers/plans/2026-05-15-kiro-streaming-implementation.md、
审查文档 docs/superpowers/plans/2026-05-15-kiro-streaming-code-review.md，
以及本次代码变更，
使用 $fix-implement（或 /fix-implement）进行方案修复及代码实现。
重点修复 HIGH-1：把 internal/runtime/executor/helps/kiro_helpers.go、internal/runtime/executor/helps/kiro_helpers_test.go 与 internal/registry/models/models.json 中新增的 Kiro 模型 alias 改动从本次 streaming 范围拆出（或正式纳入设计与实现文档），并对 "opus 4.7"（含空格）alias 重新评估必要性，必要时仅保留 opus-4.7。
```

## 8. 修复记录

### 2026-05-15 fix-implement：HIGH-1 scope 修复

**处理结论**：已修复。采用“从本次 streaming 范围拆出”的方案，不把 Kiro model alias 扩充纳入 streaming 设计与实现文档。

**代码改动**：
- `internal/runtime/executor/helps/kiro_helpers.go`：移除本轮 streaming 中误带入的 `haiku`、`opus 4.7`、`opus-4.7`、`claude-haiku-4-5-20251001` Kiro model alias 映射；保留 streaming 所需的 `KiroErrStreamRead` / `KiroErrStreamMalformed`。
- `internal/runtime/executor/helps/kiro_helpers_test.go`：移除上述 alias 的 `MapKiroModel` 用例，避免测试继续把 out-of-scope 行为固化为本轮 streaming 需求。
- `internal/registry/models/models.json`：移除 Kiro provider 段新增的 `claude-haiku-4-5-20251001`、`haiku`、`opus-4.7`、`opus 4.7` 条目。

**`"opus 4.7"` alias 评估**：
- 当前仓库证据只显示 `"opus 4.7"` 出现在本轮新增的 helper mapping、helper test、Kiro registry 条目中，没有既有调用方、文档或客户端兼容性证据要求保留含空格 model id。
- `MapKiroModel` 当前是精确 map lookup，不做大小写或前后空格归一化；把含空格 alias 暴露到 registry 还会进入公开模型列表，增加 routing key 与客户端展示的不必要兼容风险。
- 因此本轮不保留 `"opus 4.7"`。`opus-4.7` 如需支持，应在独立的 Kiro model alias 变更中重新设计、补调用方证据、文档和审查记录后再加入。

**验证结果**：
- `gofmt -w internal/runtime/executor/helps/kiro_helpers.go internal/runtime/executor/helps/kiro_helpers_test.go && go test ./internal/runtime/executor/helps -count=1 && go build -o test-output ./cmd/server && rm test-output`：PASS。
- `python3 -m json.tool internal/registry/models/models.json >/dev/null && go test ./internal/registry/... -count=1`：PASS。
- `go test ./...`：PASS。
- `rg 'opus 4\.7|opus-4\.7|haiku|claude-haiku-4-5-20251001' internal/runtime/executor/helps/kiro_helpers.go internal/runtime/executor/helps/kiro_helpers_test.go`：仅剩既有 `claude-haiku-4-5` / `claude-opus-4-7` baseline alias。
- `rg '"id": "(haiku|opus 4\.7|opus-4\.7|claude-haiku-4-5-20251001)"' internal/registry/models/models.json`：Kiro registry 段不再包含本轮新增 alias；文件开头仍存在既有 Claude provider `claude-haiku-4-5-20251001`，不属于本次 Kiro alias scope。

**剩余风险 / 复审范围**：
- 本轮只处理用户点名的 HIGH-1。§3 中 MEDIUM / LOW 建议未在本轮修复，仍可作为后续复审或独立修复项。
- HIGH-1 已关闭；代码状态可进入下一轮复审或合并前最终审查。
