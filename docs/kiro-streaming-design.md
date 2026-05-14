# Kiro 流式稳定性治理设计

  目标

  1. 消除 Kiro 流式断流被伪装成正常 message_stop。
  2. 避免 Kiro 上游无响应时无限卡住。
  3. 给 SSH/代理/网关长连接场景补齐 heartbeat 与首包失败恢复。
  4. 补齐 Kiro 流式观测，能区分：模型慢、上游静默、网络断、解析失败、客户端取消。
  5. CountTokens 不再直接 501，降低 Claude Code 上下文管理劣化。

  非目标

  - 不改变 Kiro/CodeWhisperer 上游协议。
  - 不尝试让 Kiro thinking 阶段强制输出 chunk；这是上游行为。
  - 不把 Kiro 改成 WebSocket。
  - 不做 region/proxy 自动选择，只暴露证据和配置建议。

  ---
  P0：修复断流吞错 + 首包误判

  当前问题

  streamKiroToClaudeSSE 在读流错误时只 warn + break，随后无条件补：

  - message_delta
  - message_stop

  导致网络断、上游断、解析失败都像正常完成。

  更隐蔽的问题是：当前函数一开始就 emit message_start，导致 manager 的 readStreamBootstrap
  认为首包成功，后续错误无法走 bootstrap retry / HTTP 错误路径。

  设计

  把 Kiro 流式转换从「无返回值」改为「返回错误」：

  streamKiroToClaudeSSE(ctx, body, toolNameMaps, model, opts, emit) error

  核心行为：

  1. 延迟发送 message_start
    - 不再函数开始立刻发。
    - 等解析到第一个有效 Kiro event 后，再按顺序发：
        i. message_start
      ii. 对应 content_block_start/delta 或 tool_use 事件
    - 如果首个有效 event 前发生 read error / EOF / timeout：
        - 不发送 message_start
      - 返回错误，让 bootstrap retry / auth failover 生效。
  2. 非 EOF read error 必须返回
    - readErr != nil && readErr != io.EOF：
        - 返回 kiro stream read failed: ...
      - 不补 message_stop
  3. EOF 行为区分
    - 已有有效 event 且剩余 buffer 可解析：正常补 stop。
    - 没有任何有效 event：返回 empty upstream stream 错误。
    - EOF 时存在无法解析的 partial JSON：返回 malformed/incomplete stream 错误，不补 stop。
  4. ctx cancel 直接返回
    - ctx.Done() 触发时返回 ctx.Err()。
    - 不写 terminal stop，避免把用户取消伪装成模型完成。
  5. ExecuteStream goroutine 传播错误
    - Kiro ExecuteStream 的 goroutine 捕获 streamKiroToClaudeSSE 返回值。
    - 若错误发生：
        - out <- StreamChunk{Err: err}
      - reporter.PublishFailure(ctx, err)
      - 不再 EnsurePublished 成功 usage。

  验收

  - 上游读流中途断开：Claude Code 收到 error event 或请求失败，不再正常结束。
  - 首个 Kiro event 前断开：触发 bootstrap retry / failover。
  - 正常 EOF：仍输出合法 Claude SSE，包含 message_stop。
  - 用户取消：不会被记录为成功完成。

  ---
  P0：heartbeat + chunk gap timeout

  2.1 downstream heartbeat

  已有框架支持：

  - streaming.keepalive-seconds
  - ForwardStream 会发 : keep-alive\n\n

  方案：

  1. 文档和默认推荐配置：
  streaming:
    keepalive-seconds: 15
    bootstrap-retries: 1
  2. 是否改默认值：
    - 保守方案：不改全局默认，避免影响非 Kiro SSE 客户端。
    - 推荐部署：SSH/代理场景显式开启。
    - 若要产品化：可加 Kiro-specific 默认 heartbeat，但这需要 handler 感知
  provider，改动面更大，不作为第一版。

  2.2 upstream chunk gap timeout

  这是评审补充的独立根因：Kiro HTTP client timeout 为 0，body.Read 可能永久等待。

  配置设计

  新增配置，默认建议仅作用于 Kiro：

  streaming:
    keepalive-seconds: 15
    bootstrap-retries: 1
    upstream-read-timeout-seconds: 180
    upstream-first-event-timeout-seconds: 90

  语义：

  - upstream-first-event-timeout-seconds
    - 从 Kiro HTTP 2xx 建连后，到第一个有效 Kiro event 的最大等待。
    - 超时则返回 kiro stream first event timeout。
  - upstream-read-timeout-seconds
    - 第一个 event 之后，两次 upstream read/event 间最大间隔。
    - 超时则关闭 body 并返回 kiro stream idle timeout。
  - 0 表示关闭，作为回滚开关。

  实现设计

  不要使用 http.Client.Timeout，因为它是整个请求总时长，不是 chunk gap。

  推荐实现：

  - 在 Kiro 流式转换内包一层 idle watchdog。
  - 每次成功 read 或成功解析 event 后 reset timer。
  - timeout 时：
    - 如果 body 实现 io.Closer，调用 Close() 打断阻塞 read。
    - 返回明确错误。
  - 测试里可用阻塞 reader 验证 timeout。

  验收

  - 上游首 event 前卡死超过阈值：请求失败，可重试/可观测。
  - 上游中途无 chunk 超过阈值：终端 error，不再永久卡。
  - 正常长输出每次 chunk 都会刷新 timeout，不误杀。

  ---
  P1：补齐 Kiro 流式观测

  新增日志字段

  仅记录元数据，不记录 prompt/content/tool input。

  建议事件：

  1. kiro_stream_start
    - request_id
    - provider=kiro
    - auth_id
    - model
    - region
    - stream=true
  2. kiro_stream_first_event
    - ttfb_ms
    - event_type
  3. kiro_stream_chunk_gap
    - 只在超过阈值例如 30s 时 warn
    - gap_ms
    - event_count
  4. kiro_stream_end
    - duration_ms
    - event_count
    - content_events
    - tool_events
    - stop_reason
  5. kiro_stream_error
    - duration_ms
    - ttfb_ms
    - event_count
    - error_class
    - error

  错误分类

  复用/扩展现有 Kiro error class：

  - network
  - server
  - rate_limited
  - stream_read
  - stream_timeout
  - stream_malformed
  - client_cancelled

  验收

  能从日志回答：

  - 是否拿到 Kiro HTTP 2xx？
  - 首 event 花了多久？
  - 最大 chunk gap 是多少？
  - 是上游 EOF、网络断、解析失败、timeout，还是用户取消？
  - 失败是否发生在首包前？

  ---
  P1：修正 thinking 因果与观测

  修正表述

  本地 prefix 注入不是直接制造长静默。

  准确链路是：

  1. Claude Code 请求带 thinking。
  2. Kiro executor 注入 XML prefix，触发 Kiro/CodeWhisperer thinking 模式。
  3. 上游 Kiro 在 thinking 阶段可能不回传 event。
  4. 本地无 heartbeat / 无 gap timeout 时表现为卡住或被中间链路断开。

  设计

  1. 不把 prefix 逻辑作为 P0 修复点。
  2. 加观测字段：
    - thinking_enabled
    - thinking_type
    - thinking_effort
    - thinking_budget_tokens
  3. 如果后续证据显示 thinking 长静默占比高，再考虑：
    - Kiro provider 默认降低 thinking effort。
    - 或提供 Kiro-specific payload override。
    - 但这属于 P2/P3，不进第一版。

  ---
  P2：Kiro CountTokens 兜底

  当前问题

  Kiro CountTokens 直接返回 501，Claude Code 上下文管理会退化。

  设计

  实现近似 token count：

  1. 将原请求按现有路径转成 Claude 格式。
  2. 复用项目已有 tokenizer/计数 helper，做保守估算。
  3. 返回 Claude count_tokens 兼容结构。
  4. 标记为 approximate，不要声称等于 Kiro 上游真实 token。

  验收

  - /v1/messages/count_tokens 对 Kiro 不再 501。
  - malformed request 返回 4xx。
  - 大上下文请求能返回稳定估算值。
  - 不影响正常 streaming。

  ---
  P2：region/proxy 运维建议

  设计

  代码不自动切 region。

  补充文档/日志：

  - 当前 Kiro region 来源。
  - 默认 region 为 us-east-1。
  - 日志输出 region，但不输出 token/profileArn。
  - 建议 SSH 机器执行到 Kiro endpoint 的延迟/丢包检测。

  验收

  排障时能确认：

  - 实际访问哪个 region。
  - 是否走 proxy。
  - 是否 auth-level proxy 覆盖 global proxy。

  ---
  影响面

  核心文件：

  - internal/runtime/executor/kiro_executor.go
    - 流式转换返回错误
    - 延迟 message_start
    - timeout
    - goroutine 错误传播
    - usage failure 标记
  - internal/runtime/executor/helps/kiro_helpers.go
    - 如需新增错误分类/timeout helper
  - internal/config/sdk_config.go
    - 新增 streaming upstream timeout 配置
  - config.example.yaml
    - 增加推荐配置示例
  - internal/runtime/executor/kiro_executor_test.go
    - Kiro stream 单测
  - sdk/api/handlers/handlers_stream_bootstrap_test.go
    - 首包失败 / bootstrap retry 回归

  ---
  测试矩阵

  Unit

  1. Kiro 首 event 前 read error
    - 不输出 message_start
    - 返回 error
  2. Kiro 首 event 后 read error
    - 输出已有 chunk
    - 不输出 message_stop
    - 返回 error
  3. 正常 EOF
    - 输出完整 Claude SSE
    - 包含 message_stop
  4. partial JSON EOF
    - 返回 malformed/incomplete error
  5. blocking reader timeout
    - first-event timeout 生效
    - chunk-gap timeout 生效
  6. ctx cancel
    - 返回 context.Canceled
    - 不记录成功

  Integration-ish handler test

  1. bootstrap retry
    - 第一次首包前失败
    - 第二次成功
    - 下游只看到成功流
  2. terminal error after headers
    - 下游收到 event: error
    - 不收到伪造 message_stop
  3. keepalive
    - 配置 keepalive-seconds
    - dataChan 静默期间输出 SSE comment

  Regression

  1. tool use 流仍正常。
  2. thinking block 正常。
  3. 多 tool call 顺序不变。
  4. OpenAI/Claude/Gemini executor 不受影响。

  ---
  发布与回滚

  发布

  1. 先合入 P0 + P1 观测。
  2. 在 SSH CLI 机器上灰度 Kiro provider。
  3. 推荐灰度配置：
  streaming:
    keepalive-seconds: 15
    bootstrap-retries: 1
    upstream-first-event-timeout-seconds: 90
    upstream-read-timeout-seconds: 180

  观测门槛

  灰度期间看：

  - kiro_stream_error 是否下降。
  - stream_timeout 是否出现集中爆发。
  - ttfb_ms / max_gap_ms 分布。
  - Claude Code 自动中断投诉是否下降。
  - Kiro 成功率是否低于改前。

  回滚

  1. 配置回滚：
  streaming:
    upstream-first-event-timeout-seconds: 0
    upstream-read-timeout-seconds: 0
    keepalive-seconds: 0
    bootstrap-retries: 0
  2. 代码回滚：
    - 回退 Kiro stream error propagation 相关 commit。
    - 不需要数据迁移。
    - 不影响 auth 文件格式。

  ---
  最终优先级

  1. P0：断流吞错 + 延迟 message_start + 错误传播
  2. P0：heartbeat 配置 + Kiro upstream first-event/chunk-gap timeout
  3. P1：TTFB / chunk gap / stream error 观测
  4. P2：CountTokens 近似兜底
  5. P2：region/proxy 排障文档与日志字段
