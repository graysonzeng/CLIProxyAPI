# Design: Kiro Streaming Stability Governance

- Date: 2026-05-15
- Status: Revised after design review
- Scope: M

## 1. 设计目标和范围

### 1.1 要解决的问题
- Kiro streaming 建连成功后的读流错误、上游静默关闭、解析残留和客户端取消，需要被明确区分，不能被转换成正常 `message_stop`。
- 首 payload 前失败必须接入现有 conductor `readStreamBootstrap` / `executeStreamWithModelPool` failover 与 `empty_stream` 路径，而不是依赖已经发送 payload 后的 handler terminal error。
- SSH、代理、网关长连接场景继续复用现有 downstream SSE comment heartbeat；本设计不新增 upstream post-connection timeout。
- Kiro streaming 日志和 usage 需要能区分成功、首 payload 前失败、payload 后失败和客户端取消，且不能泄露 prompt、response content、tool input、token、refresh token、profile ARN 或完整 URL。
- Kiro `CountTokens` 短期继续返回 501；approximate 方案推迟到补齐 Claude tokenizer 偏差基线与客户端兼容性证据后再评估。

### 1.2 成功标准
- 正常 Kiro 流仍保持 Claude SSE 顺序：`message_start`、content/tool block events、`message_delta`、`message_stop`。
- `message_start` 延迟到第一个有效 Kiro event 后发送；首 payload 前 EOF / 空流不发送任何 chunk，使 conductor 统一生成 retryable `empty_stream`。
- 首 payload 前 read error、malformed partial JSON 或 `ctx.Done()` 通过 `StreamChunk.Err` 上抛，使 conductor `readStreamBootstrap` 先行截获并进入 model-pool / auth failover。
- 已发送 payload 后出现 read error、malformed partial JSON 或 client cancellation 时，下游收到 terminal error 或请求取消信号，不再收到伪正常 `message_stop`。
- `reporter.EnsurePublished(ctx)` 只在 converter 成功完成且至少发送过 payload 时调用；streaming error 使用 failure 发布路径，不再记录伪成功 usage。
- 不新增 `upstream-first-event-timeout-seconds` / `upstream-read-timeout-seconds` 或任何等价 upstream watchdog 配置，遵守 `AGENTS.md:74` 的 post-connection timeout 约束。
- `/v1/messages/count_tokens` 对 Kiro 继续返回 501，并在错误信息中明确 Kiro 不支持精确 token count；不返回未验证的 approximate 字段。

### 1.3 本次范围
- `internal/runtime/executor/kiro_executor.go` 的 streaming 转换生命周期、错误传播和 usage success/failure 发布时机。
- `internal/runtime/executor/helps/kiro_helpers.go` 的 Kiro streaming-only error class 扩展；`empty_stream` 仍归 conductor 层统一 retryable code。
- 现有 handler streaming bootstrap retry、ForwardStream keepalive、conductor `readStreamBootstrap` / `wrapStreamResult` / `executeStreamWithModelPool` 的配合验证。
- Kiro executor/helper 级别单测，以及必要的 conductor / handler streaming 集成测试。

### 1.4 非目标
- 不改变 Kiro / CodeWhisperer upstream 协议、header 形状、模型映射或 auth refresh 主流程。
- 不把 Kiro streaming 改成 WebSocket。
- 不在第一版引入完整 AWS Event Stream binary frame decoder；只有 deterministic fixture 证明 brace parser 不足时才升级。
- 不新增 upstream post-connection timeout；如未来需要 idle watchdog，单独 RFC 申请把 Kiro 加入 `AGENTS.md:74` 例外列表后再做。
- 不实现 CountTokens approximate；该能力需要先补 tokenizer 偏差基线与客户端兼容性证据。
- 不改 `internal/translator/` 作为独立目标；跨协议输出继续通过现有 translator 消费 Claude SSE data lines。

## 2. 背景、约束与关键事实

### 2.1 Kiro executor 当前事实
- `KiroExecutor.CountTokens` 当前返回 501：`internal/runtime/executor/kiro_executor.go:66`-`:69`。
- `ExecuteStream` 当前启动 goroutine 调 `streamKiroToClaudeSSE`，该函数无返回值，goroutine 最后无条件 `reporter.EnsurePublished(ctx)`：`internal/runtime/executor/kiro_executor.go:276`-`:309`。
- `streamKiroToClaudeSSE` 当前签名无返回值：`internal/runtime/executor/kiro_executor.go:1152`。
- `streamKiroToClaudeSSE` 当前在读取 upstream body 前发送 `message_start`：`internal/runtime/executor/kiro_executor.go:1175`-`:1186`。
- 读流中非 EOF 错误当前只 `Warnf` 后 `break`：`internal/runtime/executor/kiro_executor.go:1357`-`:1361`。
- 循环结束后当前无条件补 `message_delta` 和 `message_stop`：`internal/runtime/executor/kiro_executor.go:1388`-`:1398`。
- Kiro HTTP request 使用 proxy-aware client 且 post-connection timeout 为 0：`internal/runtime/executor/kiro_executor.go:514`。

### 2.2 Conductor / handler 现有失败覆盖事实
- `readStreamBootstrap` 会先读取 stream 的第一个 payload chunk 或 err；首 payload 前 `chunk.Err` 会立即返回给 model-pool failover：`sdk/cliproxy/auth/conductor.go:786`-`:816`。
- `executeStreamWithModelPool` 在 model pool 内对 bootstrap error 做失败标记和下一个 model failover；都失败时才返回 `newStreamBootstrapError`：`sdk/cliproxy/auth/conductor.go:867`-`:933`。
- 上游在首 payload 前关闭且无 buffered payload 时，conductor 已有 retryable `empty_stream`：`sdk/cliproxy/auth/conductor.go:936`-`:944`。
- `wrapStreamResult` 在 payload 后看到 `chunk.Err` 会调用 `MarkResult(Success: false)`，否则 stream 完整结束后才 `MarkResult(Success: true)`：`sdk/cliproxy/auth/conductor.go:818`-`:864`。
- Handler 层 `bootstrap-retries` 只在 conductor 已经返回 stream 后、且 handler 尚未发送 payload 时再次看到 `chunk.Err` 才触发：`sdk/api/handlers/handlers.go:756`-`:772`。本设计以 conductor failover 为主，handler bootstrap-retries 为辅。

### 2.3 Timeout 与 tokenizer 约束
- `AGENTS.md:74` 明确约束：credential acquisition 之外，upstream connection 建立后不得为后续网络行为设置 timeout；当前允许例外不包含 Kiro。
- 现有 server streaming 配置只有 downstream `keepalive-seconds` 与 handler `bootstrap-retries`：`internal/config/sdk_config.go:49`-`:59`。
- `ForwardStream` 已支持 SSE comment heartbeat，默认写 `: keep-alive\n\n`：`sdk/api/handlers/stream_forwarder.go:45`-`:49`。
- 项目 tokenizer helper 只有 OpenAI-style tokenizer（`cl100k_base` / GPT / o200k 系列），不存在 Claude 原生 tokenizer：`internal/runtime/executor/helps/token_helpers.go:11`-`:37`。
- Claude 精确 token count 走 Anthropic `/v1/messages/count_tokens` 服务端接口：`internal/runtime/executor/claude_executor.go:583`-`:638`；Kiro 没有等价 upstream 端点。

## 3. 根因与影响

### 3.1 是否需要根因分析
- 本设计不是单一线上事故 RCA，但设计依赖的关键事实必须被核对。
- 隐式根因是：Kiro converter 在拿到任何 upstream event 前就发送 downstream payload，后续读流失败又被吞掉，最终无条件补成功 terminal events 和 success usage。

### 3.2 已确认根因链
1. 首 payload 过早发送：`message_start` 在读 body 前发送，导致首事件前失败不再是 conductor 可安全 failover 的 bootstrap failure。
2. 错误吞掉：非 EOF read error 只记录 warn 并退出循环，没有进入 `StreamChunk.Err`。
3. 伪成功：循环结束后无条件 `message_delta` / `message_stop`，goroutine 无条件 `EnsurePublished`。
4. 证据链缺口：旧设计把恢复重点放到 handler `bootstrap-retries`，但真实主路径应是 conductor `readStreamBootstrap` / `executeStreamWithModelPool` / `empty_stream`。

### 3.3 未确认假设
- Kiro upstream 在 thinking 阶段的真实静默分布、最长 chunk gap 和不同 region/proxy 的失败占比需要后续 telemetry / canary 采集确认。
- Kiro brace-count parser 是否覆盖所有真实 AWS Event Stream binary frame 形态，需要 P0 fake-upstream fixture 判定；本设计不假设必须重写 parser。
- Claude approximate token count 是否能被 Claude Code / Cursor 等客户端安全消费，需要客户端兼容性测试与 tokenizer 偏差基线。

### 3.4 对设计的影响
- 先修 stream lifecycle、错误传播和 usage 成败语义；telemetry、parser decoder 和 CountTokens approximate 作为后续独立迭代。
- 不使用 timeout 解决 upstream 静默；本轮只处理 EOF、read error、parse residue 与 context cancellation。
- 首 payload 前 EOF/空流复用 conductor `empty_stream`；Kiro 不平行定义同名错误码。

## 4. 方案对比

### 4.1 方案 A：最小错误返回补丁
- 核心思路：让 `streamKiroToClaudeSSE` 返回 error；read error 时上抛，正常 EOF 时维持现有 terminal events。
- 优点：改动最小，能修复中途 read error 被吞的问题。
- 缺点：如果仍然在首事件前发送 `message_start`，首包恢复仍然绕过 conductor bootstrap failover；也不能修复 success usage 伪装。

### 4.2 方案 B：Kiro streaming lifecycle wrapper（推荐）
- 核心思路：把 Kiro stream 转换改成显式状态机：首有效 event 后才发 `message_start`；EOF、read error、parse residue、ctx cancel 按 payload 是否已开始分别返回明确结果；`ExecuteStream` goroutine 按结果发布 success 或 failure。
- 优点：契合 conductor `readStreamBootstrap` / `empty_stream` / `wrapStreamResult`；错误语义和 usage 记录一致；不需要立刻重写 parser；不新增 timeout。
- 缺点：需要调整现有 streaming 单测的 `message_start` 时机断言；需要补 conductor / handler 集成测试。

### 4.3 方案 C：先实现完整 AWS Event Stream decoder
- 核心思路：在修 lifecycle 前先替换 brace parser 为 frame-aware decoder，带 CRC、bounded buffer 和 decoder states。
- 优点：协议正确性最强。
- 缺点：实现与回归面更大，且当前主链路是错误吞掉和 terminal success 伪装，不一定由 parser 引发。

### 4.4 选型结论
- 选择：方案 B。
- 理由：方案 B 直接修复首包恢复、断流吞错、取消语义和 usage/telemetry 一致性，同时遵守 timeout 约束，并保留 parser 与 CountTokens 的后续升级 gate。

## 5. 详细方案

### 5.1 核心思路
- 将 `streamKiroToClaudeSSE(ctx, body, toolNameMaps, model, emit)` 改为返回 `kiroStreamResult, error`。
- `kiroStreamResult` 至少记录 `eventCount` 与 `payloadStarted`；第一版不强制落完整 telemetry 字段。
- `message_start` 延迟到第一个有效 Kiro event 被解析后发送。
- 正常 EOF 且 `payloadStarted=true` 时，关闭未关闭 block，发送 `message_delta` 和 `message_stop`，返回 success。
- 正常 EOF 且 `eventCount=0/payloadStarted=false` 时，不发送 payload、不发布 success usage，让 conductor 走已有 `empty_stream`。
- 非 EOF read error、parse residue、ctx cancel：返回 error；若已发送 payload，则不补 `message_stop`。
- `CountTokens` 保持 501；错误信息可标识 `cpa_kiro_count_tokens_unsupported`，但不新增 approximate 响应字段。

### 5.2 关键数据流 / 控制流
1. Handler 调 `AuthManager.ExecuteStream`，进入 conductor。
2. Kiro `ExecuteStream` 完成 request translation、thinking pipeline、`buildKiroCodeWhispererRequest` 和 `sendKiroRequest`。
3. Kiro goroutine 调新的 stream lifecycle converter。
4. Converter 读取 upstream body，解析 `helps.ParseAwsEventStreamBuffer` 返回的有效事件。
5. 第一个有效事件到达时，converter 发送 `message_start`，再发送对应 content/tool SSE。
6. 正常 EOF 且已发送 payload 时，converter 发送 success terminal events 并返回 nil。
7. 首 payload 前 EOF / 空流：converter 不发送任何 chunk，goroutine 不发布 success usage，channel 关闭；conductor `readStreamBootstrap` 返回 `closed && len(buffered)==0`，进入 retryable `empty_stream`。
8. 首 payload 前 read error / malformed / cancel：goroutine 发送 `StreamChunk{Err}`；conductor `readStreamBootstrap` 先截获，`executeStreamWithModelPool` 在 model pool 内 failover，都失败时上层再切换 auth/credential。
9. 已发送 payload 后失败：`wrapStreamResult` 转发 `chunk.Err` 并 `MarkResult(Success:false)`；handler 通过现有 terminal error writer 输出客户端格式对应错误。handler `bootstrap-retries` 只作为 conductor 之后的兜底路径。

### 5.3 接口 / 配置 / 数据结构变更
- 接口：不新增 HTTP route，不改变外部 Kiro request/response API。Streaming error 的下游格式沿用现有 handler terminal error writer。
- 配置：不新增 upstream timeout 配置；保留现有全局 server streaming 配置：
  - `streaming.keepalive-seconds`：downstream SSE comment heartbeat，默认 `0` 关闭。
  - `streaming.bootstrap-retries`：handler 层首 payload 前兜底重试，默认 `0`；主恢复路径仍是 conductor。
- 数据结构：
  - 扩展 `helps.KiroErrorClass` 的 streaming-only 子分类，例如 `stream_read`、`stream_malformed`、`stream_cancelled`。
  - 不新增 Kiro-side `empty_stream` class；首 payload 前 EOF/空流复用 conductor `empty_stream` retryable code。
  - 可新增 executor-local `kiroStreamResult`，字段只包含元数据：`event_count`、`payload_started`。

### 5.4 错误处理与回退策略
- 首事件前 `io.EOF` / 空流：不返回 Kiro-side error、不发送 payload、不发布 success usage；交由 conductor `empty_stream` 统一处理。
- 首事件后 `io.EOF` 且 remaining 为空或可完全解析：正常 terminal success。
- 非 EOF read error：返回 `KiroErrStreamRead`，不补 `message_stop`。
- EOF 时存在无法解析的 partial JSON：返回 `KiroErrStreamMalformed`，不补 `message_stop`。
- `ctx.Done()`：返回 `ctx.Err()` 或 `KiroErrStreamCancelled` 分类；不记录 success usage。
- 不实现 first-event timeout 或 read idle timeout。客户端取消依赖 `http.NewRequestWithContext(ctx, ...)` 和读循环中的 `ctx.Done()` 检查。

### 5.5 风险与缓解
- 风险：延迟 `message_start` 改变现有单测和部分客户端对首字节的期待。
  - 缓解：downstream heartbeat 可由 handler 层维持连接；测试断言改成“首有效 event 后才发 message_start”，并覆盖首事件前失败不会发送 payload。
- 风险：不新增 upstream timeout 后，真实长 thinking 静默仍可能长期挂起。
  - 缓解：这是 `AGENTS.md:74` 明确约束下的取舍；若要改变，单独 RFC 申请 Kiro timeout 例外。
- 风险：parser residual 判断误报 malformed。
  - 缓解：只在 EOF 后最终 parse 仍存在以 `{` 开头的 incomplete JSON 时判 malformed；其他 binary header/junk 保留到 fixture 判定。
- 风险：`chunk.Err` 上抛会触发 conductor `MarkResult(Success:false)` / cooldown 计数器。
  - 缓解：这是预期行为；本次不调整 cooldown 阈值，只修复伪成功。
- 风险：CountTokens 继续 501 可能让部分客户端上下文管理退化。
  - 缓解：短期保持诚实失败，避免错误 approximate 误导 compaction；中期补客户端兼容性与偏差基线后再设计。

## 6. 验证计划
- Unit：`streamKiroToClaudeSSE` 正常 text/tool/thinking 流顺序，延迟 `message_start`，正常 EOF 发送 stop。
- Unit：首事件前 EOF / 空流不 emit payload，converter 不发布 success usage，conductor 可进入 `empty_stream`。
- Unit：首事件前 read error、ctx cancel、malformed partial JSON 均不 emit payload，并返回可被 conductor bootstrap 捕获的 error。
- Unit：首事件后 read error、ctx cancel、malformed partial JSON 均不发送 `message_stop`。
- Integration：`KiroExecutor.ExecuteStream` fake upstream 覆盖首事件前断开，conductor `readStreamBootstrap` / `empty_stream` / model-pool failover 生效。
- Integration：已发送 payload 后断流，handler 输出 terminal error，不记录 success usage。
- Integration：跨协议 terminal error 覆盖 OpenAI chat、Gemini stream、Responses event-stream 三种格式。
- Integration：`wrapStreamResult` 在 `chunk.Err` 出现时调用 `MarkResult(Success:false)`。
- CountTokens：Kiro `CountTokens` 保持 501；不走 translator-only approximate 改动。
- Build：Go 改动后执行 `gofmt -w .`、focused tests、`go build -o test-output ./cmd/server && rm test-output`；若触及 shared provider routing、translator、SDK 行为，再执行 `go test ./...`。

## 7. 关键决策摘要
- 选择 Kiro-local streaming lifecycle wrapper，不先做完整 AWS Event Stream decoder。
- `message_start` 延迟到第一个有效 Kiro event 后发送，以恢复首 payload 前 conductor bootstrap failover 能力。
- 成功 terminal events 只允许在正常 EOF 且已发送 payload 后发送。
- 不新增 upstream first-event/read timeout；如未来需要，独立 RFC 申请 `AGENTS.md:74` 例外。
- Downstream heartbeat 复用现有 `ForwardStream`，不为第一版新增 Kiro-specific handler default。
- CountTokens 短期继续 501；approximate 推迟到补齐 Claude tokenizer 偏差基线与客户端兼容性证据后再做。

## 8. 修订记录

### 2026-05-15 design-review 修订
- 采纳 HIGH-1：删除 upstream timeout 配置与 timeout 错误路径；明确遵守 `AGENTS.md:74`，如需 Kiro watchdog 必须独立 RFC 申请例外。
- 采纳 HIGH-2：补齐 conductor `readStreamBootstrap` / `wrapStreamResult` / `executeStreamWithModelPool` / `empty_stream` 证据，重写错误传播为 conductor 失败覆盖为主、handler `bootstrap-retries` 为辅。
- 采纳 HIGH-3：CountTokens 短期保留 501，approximate 推迟到 tokenizer 偏差基线与客户端兼容性证据后再做。
- 采纳 MEDIUM-1：Kiro streaming error class 作为 `KiroErrorClass` 子分类；`empty_stream` 复用 conductor code。
- 采纳 MEDIUM-2：本轮不新增 Kiro timeout 配置；若未来例外通过，必须放入 Kiro-only 命名空间。
- 采纳 MEDIUM-3 / LOW-3：验证计划补跨协议 terminal error、conductor failover、`MarkResult(Success:false)` 场景。

## 9. Handoff

### 9.1 同会话继续
`直接执行 $design-implement 或 /design-implement`

### 9.2 新会话恢复 prompt
```text
请阅读设计文档 docs/kiro-streaming-design.md
以及评审文档 docs/superpowers/plans/2026-05-15-kiro-streaming-design-review.md，
重点核对根因证据补全（conductor readStreamBootstrap / wrapStreamResult / empty_stream 路径、AGENTS.md 第 74 行 timeout 约束、Claude tokenizer 缺失）以及方案修订点，
使用 $design-implement（或 /design-implement）进行方案修订及实现。
重点关注：HIGH-1：删除 upstream timeout 配置或单独申请 AGENTS.md 例外；HIGH-2：错误传播以 conductor 失败覆盖为主，handler bootstrap-retries 为辅；HIGH-3：CountTokens 短期保留 501，approximate 推迟到补齐 tokenizer 偏差基线 + 客户端兼容性证据后再做。
```
