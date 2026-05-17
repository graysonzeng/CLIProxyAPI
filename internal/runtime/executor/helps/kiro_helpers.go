package helps

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Kiro protocol constants aligned with AIClient2API source.
// URL templates are var (not const) to allow test-time patching via httptest.
var (
	KiroSocialRefreshURLTemplate = "https://prod.{{region}}.auth.desktop.kiro.dev/refreshToken"
	KiroIDCRefreshURLTemplate    = "https://oidc.{{region}}.amazonaws.com/token"
	KiroBaseURLTemplate          = "https://q.{{region}}.amazonaws.com/generateAssistantResponse"
	KiroUsageLimitsURLTemplate   = "https://q.{{region}}.amazonaws.com/getUsageLimits"
)

const (
	KiroVersion              = "0.11.63"
	KiroContentType          = "application/json"
	KiroAcceptJSON           = "application/json"
	KiroOriginAIEditor       = "AI_EDITOR"
	KiroResourceAgentic      = "AGENTIC_REQUEST"
	KiroChatTrigger          = "MANUAL"
	KiroAgentTaskType        = "vibe"
	KiroDefaultRegion        = "us-east-1"
	KiroIDCRegion            = "us-east-1"
	KiroMaxToolNameLength    = 64
	KiroMaxDescriptionLength = 9216
	KiroAuthMethodSocial     = "social"

	KiroThinkingStartTag = "<thinking>"
	KiroThinkingEndTag   = "</thinking>"
	KiroThinkingModeTag  = "<thinking_mode>"
	KiroMaxLenTag        = "<max_thinking_length>"
	KiroEffortTag        = "<thinking_effort>"

	KiroMinBudgetTokens     = 1024
	KiroMaxBudgetTokens     = 24576
	KiroDefaultBudgetTokens = 20000
)

// KiroModelMapping maps client-facing model IDs to upstream Kiro (CodeWhisperer) model IDs.
//
// The map MUST stay in sync with the kiro section of
// internal/registry/models/models.json: the registry decides whether a request
// can be routed to Kiro at all (see internal/util.GetProviderName), and this
// map decides how that routed request is translated for the upstream API.
// Missing a date-suffixed alias here while exposing it in the registry
// surfaces as 502 "unknown provider for model" for Claude Code, since Claude
// Code uses the dated form by default for auto-routed light-tier traffic
// (completion summary, telemetry, internal subagent calls).
//
// Keep the explicit, undated Haiku alias mapped to Kiro Haiku for operators
// that choose it directly. The dated Haiku alias is Claude Code's auto-routed
// light-tier model, and is intentionally upgraded to Sonnet 4.6 because Kiro
// Haiku frequently emits incomplete tool input shards that Claude Code cannot
// execute reliably.
var KiroModelMapping = map[string]string{
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4-5-20251001":  "claude-sonnet-4.6",
	"claude-opus-4-7":            "claude-opus-4.7",
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-sonnet-4-6":          "claude-sonnet-4.6",
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
}

// kiroAdaptiveLevelsModels is the set of upstream CodeWhisperer model ids
// that understand the <thinking_mode>adaptive</thinking_mode> +
// <thinking_effort>low|medium|high</thinking_effort> protocol. Older Kiro
// tiers (haiku 4.5, sonnet 4.5, opus 4.5) only honor enabled/disabled
// thinking; sending adaptive prefixes to them is silently ignored or coerced
// upstream, while still costing the request a thinking pass we can't
// observe. See KiroSupportsAdaptiveLevels.
var kiroAdaptiveLevelsModels = map[string]struct{}{
	"claude-sonnet-4.6": {},
	"claude-opus-4.6":   {},
	"claude-opus-4.7":   {},
}

// KiroSupportsAdaptiveLevels reports whether the upstream CodeWhisperer model
// id (the value returned by MapKiroModel, NOT the client-facing alias)
// understands adaptive thinking with effort levels. Used by
// kiro_executor.buildKiroCodeWhispererRequest to skip the proxy's default
// `enabled+budget → adaptive+effort` rewrite on lighter tiers, where the
// rewrite would just slow the request down without producing usable
// reasoning content.
func KiroSupportsAdaptiveLevels(upstreamModel string) bool {
	_, ok := kiroAdaptiveLevelsModels[strings.TrimSpace(upstreamModel)]
	return ok
}

// KiroUsageLimitsURL builds the Kiro quota endpoint URL.
func KiroUsageLimitsURL(region, profileArn string) string {
	region = strings.TrimSpace(region)
	if region == "" {
		region = KiroDefaultRegion
	}
	base := strings.ReplaceAll(KiroUsageLimitsURLTemplate, "{{region}}", region)
	values := url.Values{}
	values.Set("origin", KiroOriginAIEditor)
	values.Set("resourceType", KiroResourceAgentic)
	values.Set("isEmailRequired", "true")
	if profileArn = strings.TrimSpace(profileArn); profileArn != "" {
		values.Set("profileArn", profileArn)
	}
	return base + "?" + values.Encode()
}

// MapKiroModel returns the upstream CodeWhisperer model ID for the given client model.
// Unknown models are passed through unchanged.
func MapKiroModel(model string) string {
	if mapped, ok := KiroModelMapping[model]; ok {
		return mapped
	}
	return model
}

// --- Tool name shortening / mapping ---

// ShortenKiroToolName truncates a tool name to KiroMaxToolNameLength.
// If truncation is needed, a SHA-256 hash suffix is appended.
func ShortenKiroToolName(name string) string {
	if len(name) <= KiroMaxToolNameLength {
		return name
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:12]
	prefixLen := KiroMaxToolNameLength - len(hash) - 1
	return name[:prefixLen] + "_" + hash
}

// KiroToolNameMaps holds bidirectional mapping between original and shortened tool names.
type KiroToolNameMaps struct {
	aliasToOriginal map[string]string
	originalToAlias map[string]string
}

// BuildKiroToolNameMaps creates tool name maps from Claude tool definitions.
func BuildKiroToolNameMaps(tools []json.RawMessage) *KiroToolNameMaps {
	m := &KiroToolNameMaps{
		aliasToOriginal: make(map[string]string),
		originalToAlias: make(map[string]string),
	}
	for _, raw := range tools {
		var t struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &t); err != nil || t.Name == "" {
			continue
		}
		alias := ShortenKiroToolName(t.Name)
		m.originalToAlias[t.Name] = alias
		if alias != t.Name {
			m.aliasToOriginal[alias] = t.Name
		}
	}
	return m
}

// ToKiroName returns the shortened name for a given original tool name.
func (m *KiroToolNameMaps) ToKiroName(name string) string {
	if m == nil {
		return ShortenKiroToolName(name)
	}
	if alias, ok := m.originalToAlias[name]; ok {
		return alias
	}
	return ShortenKiroToolName(name)
}

// FromKiroName restores the original name from a shortened Kiro name.
func (m *KiroToolNameMaps) FromKiroName(name string) string {
	if m == nil {
		return name
	}
	if original, ok := m.aliasToOriginal[name]; ok {
		return original
	}
	return name
}

// --- Machine ID ---

// GenerateKiroMachineID creates a deterministic machine ID from auth credentials.
func GenerateKiroMachineID(authUUID, profileArn, clientID string) string {
	key := authUUID
	if key == "" {
		key = profileArn
	}
	if key == "" {
		key = clientID
	}
	if key == "" {
		key = "KIRO_DEFAULT_MACHINE"
	}
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h)
}

// --- OS info for user-agent ---

var (
	osInfoOnce sync.Once
	osName     string
	goVersion  string
)

func initOSInfo() {
	osInfoOnce.Do(func() {
		goVersion = runtime.Version()
		platform := runtime.GOOS
		switch platform {
		case "darwin":
			osName = "macos"
		case "windows":
			osName = "windows"
		default:
			osName = platform
		}
		if hostname, err := os.Hostname(); err == nil && hostname != "" {
			_ = hostname // available if needed
		}
	})
}

// KiroRequestHeaders returns the default headers required by the Kiro API.
// machineID should be generated via GenerateKiroMachineID.
//
// NOTE: Earlier versions sent `Connection: close` here, copied verbatim from
// the AIClient2API and kiro.rs reference implementations without a documented
// justification. That forced a fresh TCP+TLS handshake on every request to
// the AWS CodeWhisperer endpoint, which from typical Asia hosts costs roughly
// 200–500ms per request (≈3 RTTs to us-east-1). The header has been removed
// so Go's HTTP transport can reuse pooled keep-alive connections; AWS LBs
// honor keep-alive by default. Combined with KiroSharedHTTPClient below, this
// is the single biggest TTFT win available without changing thinking strength.
func KiroRequestHeaders(machineID string) map[string]string {
	initOSInfo()
	return map[string]string{
		"Content-Type":                KiroContentType,
		"Accept":                      KiroAcceptJSON,
		"amz-sdk-request":             "attempt=1; max=3",
		"x-amzn-codewhisperer-optout": "true",
		"x-amzn-kiro-agent-mode":      "vibe",
		"x-amz-user-agent":            fmt.Sprintf("aws-sdk-js/1.0.34 KiroIDE-%s-%s", KiroVersion, machineID),
		"user-agent":                  fmt.Sprintf("aws-sdk-js/1.0.34 ua/2.1 os/%s lang/go api/codewhispererstreaming#1.0.34 m/E KiroIDE-%s-%s", osName, KiroVersion, machineID),
	}
}

// kiroSharedClientOnce ensures the singleton Kiro HTTP client and its tuned
// transport are constructed exactly once for the process lifetime.
var (
	kiroSharedClientOnce sync.Once
	kiroSharedClient     *http.Client
)

// KiroSharedHTTPClient returns a process-wide *http.Client tuned for the AWS
// CodeWhisperer streaming endpoint. The client must NOT be used when the
// caller has a per-request proxy URL or context-injected RoundTripper; those
// paths still need a fresh transport (see kiro_executor.httpClientFor).
//
// Tuning rationale:
//   - MaxIdleConnsPerHost=16: Kiro typically routes traffic from a small set
//     of accounts to one AWS region. 16 idle conns per host gives Claude
//     Code subagent fan-outs of 8+ concurrent requests room to multiplex
//     across HTTP/2 streams without forcing the transport to dial a fresh
//     TCP+TLS connection mid-burst (which surfaced as a ~1.4s p99 TTFT
//     outlier on 8x concurrent benchmark runs at the previous 8 cap).
//     16 idle sockets is still trivial: ~8 fds per active region.
//   - IdleConnTimeout=5m: AWS LB idle timeout is 60s by default; 5m on the
//     client side combined with the LB timeout means the client may send a
//     request on a half-closed conn occasionally and transparently retry.
//     Lowering further (e.g. 50s) is also reasonable and can be a follow-up.
//   - TLSHandshakeTimeout=10s: only applies during credential acquisition,
//     which the project's "no post-connection timeouts" rule explicitly allows.
//   - ExpectContinueTimeout=1s: matches Go default; benign for our requests.
//   - ForceAttemptHTTP2=true: matches Go default since 1.17. Enables HTTP/2
//     multiplexing across concurrent requests on a single TCP connection.
//   - TLS ClientSessionCache: when keep-alive does miss (long idle, network
//     blip), session resumption brings the next handshake from ~3 RTTs down
//     to ~1 RTT, saving ~150-300ms on Asia → us-east-1 paths.
//   - Proxy=http.ProxyFromEnvironment: matches http.DefaultTransport so the
//     HTTPS_PROXY / HTTP_PROXY env vars keep working for operators who rely
//     on shell-level proxies (per-request proxy URLs already get a fresh
//     transport, see httpClientFor).
//
// No timeout is set on the *http.Client itself: streaming responses can take
// many minutes and the project rule forbids post-handshake deadlines.
func KiroSharedHTTPClient() *http.Client {
	kiroSharedClientOnce.Do(func() {
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       5 * time.Minute,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				ClientSessionCache: tls.NewLRUClientSessionCache(64),
				MinVersion:         tls.VersionTLS12,
			},
		}
		kiroSharedClient = &http.Client{Transport: transport}
	})
	return kiroSharedClient
}

// KiroPerRequestHeaders returns per-request headers (Authorization + invocation id).
func KiroPerRequestHeaders(accessToken string) map[string]string {
	return map[string]string{
		"Authorization":         "Bearer " + accessToken,
		"amz-sdk-invocation-id": uuid.New().String(),
	}
}

// --- Refresh URL helpers ---

// KiroSocialRefreshURL returns the social auth refresh URL for the given region.
func KiroSocialRefreshURL(region string) string {
	if region == "" {
		region = KiroDefaultRegion
	}
	return strings.ReplaceAll(KiroSocialRefreshURLTemplate, "{{region}}", region)
}

// KiroIDCRefreshURL returns the builder-id refresh URL for the given region.
func KiroIDCRefreshURL(region string) string {
	if region == "" {
		region = KiroIDCRegion
	}
	return strings.ReplaceAll(KiroIDCRefreshURLTemplate, "{{region}}", region)
}

// KiroBaseURL returns the generateAssistantResponse endpoint for the given region.
func KiroBaseURL(region string) string {
	if region == "" {
		region = KiroDefaultRegion
	}
	return strings.ReplaceAll(KiroBaseURLTemplate, "{{region}}", region)
}

// --- AWS Event Stream parser ---

// KiroStreamEvent represents a parsed event from the Kiro AWS Event Stream.
type KiroStreamEvent struct {
	Type string // "content", "toolUse", "toolUseInput", "toolUseStop", "contextUsage", "exception"
	// Content holds the text for "content" events.
	Content string
	// ToolUse fields (populated for toolUse/toolUseInput/toolUseStop events).
	ToolName  string
	ToolUseID string
	ToolInput string
	ToolStop  bool
	// ContextUsage fields.
	ContextUsagePercentage float64
	// Exception fields.
	ExceptionType string
	Message       string
}

// ParseAwsEventStreamBuffer extracts JSON events from an AWS Event Stream buffer.
// It returns the parsed events and any remaining unparsed data as a sub-slice
// of the input (no copy).
//
// This mirrors AIClient2API's parseAwsEventStreamBuffer using brace-counting
// to handle nested JSON and binary frame headers.
//
// The function operates on []byte so that streaming callers can grow a single
// backing buffer with append() across many body.Read calls without paying the
// O(N²) `string += string(buf[:n])` cost the previous string-based API forced.
// The returned `remaining` sub-slice is safe to feed straight back into
// append(buf[:0], remaining...) on the next round, which the streaming caller
// in streamKiroToClaudeSSE relies on.
func ParseAwsEventStreamBuffer(buffer []byte) (events []KiroStreamEvent, remaining []byte) {
	remaining = buffer
	searchStart := 0

	for {
		rel := bytes.IndexByte(remaining[searchStart:], '{')
		if rel < 0 {
			break
		}
		jsonStart := searchStart + rel

		// Brace-counting JSON extraction (handles nested objects and strings).
		braceCount := 0
		jsonEnd := -1
		inString := false
		escapeNext := false

		for i := jsonStart; i < len(remaining); i++ {
			ch := remaining[i]
			if escapeNext {
				escapeNext = false
				continue
			}
			if ch == '\\' {
				escapeNext = true
				continue
			}
			if ch == '"' {
				inString = !inString
				continue
			}
			if !inString {
				if ch == '{' {
					braceCount++
				} else if ch == '}' {
					braceCount--
					if braceCount == 0 {
						jsonEnd = i
						break
					}
				}
			}
		}

		if jsonEnd < 0 {
			// Incomplete JSON — keep from jsonStart onward as a sub-slice so
			// the caller can append the next read into the same backing array.
			remaining = remaining[jsonStart:]
			return events, remaining
		}

		jsonBytes := remaining[jsonStart : jsonEnd+1]

		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(jsonBytes, &parsed); err != nil {
			// JSON parse failed — skip this "{" and continue.
			searchStart = jsonStart + 1
			continue
		}

		evt := classifyKiroEvent(parsed)
		if evt != nil {
			events = append(events, *evt)
		}

		searchStart = jsonEnd + 1
		if searchStart >= len(remaining) {
			remaining = remaining[len(remaining):]
			return events, remaining
		}
	}

	// Trim consumed prefix; returned slice is still backed by the input array.
	if searchStart > 0 && len(remaining) > 0 {
		remaining = remaining[searchStart:]
	}
	if evt := classifyKiroRawException(remaining); evt != nil {
		events = append(events, *evt)
		remaining = remaining[len(remaining):]
	}
	return events, remaining
}

func classifyKiroRawException(raw []byte) *KiroStreamEvent {
	if len(raw) == 0 {
		return nil
	}
	switch {
	case bytes.Contains(raw, []byte("ContentLengthExceededException")):
		return &KiroStreamEvent{Type: "exception", ExceptionType: "ContentLengthExceededException", Message: string(raw)}
	default:
		return nil
	}
}

func classifyKiroEvent(parsed map[string]json.RawMessage) *KiroStreamEvent {
	// Check for followupPrompt — skip these.
	if _, ok := parsed["followupPrompt"]; ok {
		return nil
	}

	hasContent := false
	hasName := false
	hasInput := false
	hasStop := false
	hasContextUsage := false

	_, hasContent = parsed["content"]
	_, hasName = parsed["name"]
	_, hasInput = parsed["input"]
	_, hasStop = parsed["stop"]
	_, hasContextUsage = parsed["contextUsagePercentage"]

	// toolUse start: has name + toolUseId
	if hasName {
		var name, toolUseID string
		_ = json.Unmarshal(parsed["name"], &name)
		if raw, ok := parsed["toolUseId"]; ok {
			_ = json.Unmarshal(raw, &toolUseID)
		}
		var input string
		if hasInput {
			input = normalizeKiroToolInput(parsed["input"])
		}
		var stop bool
		if hasStop {
			_ = json.Unmarshal(parsed["stop"], &stop)
		}
		return &KiroStreamEvent{
			Type:      "toolUse",
			ToolName:  name,
			ToolUseID: toolUseID,
			ToolInput: input,
			ToolStop:  stop,
		}
	}

	// toolUseInput: has input but no name
	if hasInput && !hasName {
		var toolUseID string
		if raw, ok := parsed["toolUseId"]; ok {
			_ = json.Unmarshal(raw, &toolUseID)
		}
		return &KiroStreamEvent{
			Type:      "toolUseInput",
			ToolUseID: toolUseID,
			ToolInput: normalizeKiroToolInput(parsed["input"]),
		}
	}

	// toolUseStop: has stop but no contextUsagePercentage
	if hasStop && !hasContextUsage {
		var toolUseID string
		if raw, ok := parsed["toolUseId"]; ok {
			_ = json.Unmarshal(raw, &toolUseID)
		}
		var stop bool
		_ = json.Unmarshal(parsed["stop"], &stop)
		return &KiroStreamEvent{
			Type:      "toolUseStop",
			ToolUseID: toolUseID,
			ToolStop:  stop,
		}
	}

	// contextUsage
	if hasContextUsage {
		var pct float64
		_ = json.Unmarshal(parsed["contextUsagePercentage"], &pct)
		return &KiroStreamEvent{
			Type:                   "contextUsage",
			ContextUsagePercentage: pct,
		}
	}

	// content event
	if hasContent {
		var content string
		_ = json.Unmarshal(parsed["content"], &content)
		return &KiroStreamEvent{
			Type:    "content",
			Content: content,
		}
	}

	return nil
}

func normalizeKiroToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try as string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Otherwise return the raw JSON text.
	return string(raw)
}

// --- Thinking prefix generation ---

// GenerateKiroThinkingPrefix builds the thinking XML prefix for the system prompt.
func GenerateKiroThinkingPrefix(thinkingType string, budgetTokens int, effort string) string {
	t := strings.ToLower(strings.TrimSpace(thinkingType))
	switch t {
	case "enabled":
		budget := normalizeThinkingBudget(budgetTokens)
		return fmt.Sprintf("%senabled</thinking_mode>%s%d</max_thinking_length>", KiroThinkingModeTag, KiroMaxLenTag, budget)
	case "adaptive":
		e := strings.ToLower(strings.TrimSpace(effort))
		if e != "low" && e != "medium" && e != "high" {
			e = "high"
		}
		return fmt.Sprintf("%sadaptive</thinking_mode>%s%s</thinking_effort>", KiroThinkingModeTag, KiroEffortTag, e)
	default:
		return ""
	}
}

// KiroThinkingEffortPreserve mirrors config.KiroThinkingEffortPreserve so the
// helps package does not import internal/config. It signals that
// SelectKiroThinkingPrefix should forward enabled+budget requests verbatim
// instead of rewriting them into adaptive thinking.
const KiroThinkingEffortPreserve = "preserve"

// KiroDefaultThinkingEffort is the effort level applied when defaultEffort is
// unset or invalid. See config.KiroConfig.DefaultThinkingEffort and the data
// behind the choice (medium == 100% completion, ~3.1s TTFT, retains adaptive
// reasoning for Opus 4.6 today).
const KiroDefaultThinkingEffort = "medium"

// SelectKiroThinkingPrefix decides which thinking_mode prefix to inject for a
// Kiro request, given the client-supplied thinking config and the proxy's
// configured default effort.
//
// The rewrite is necessary because Kiro Opus 4.6 has been measured to drop
// visible content roughly 25-50% of the time when reasoning under high token
// budgets (the Claude Code default sends thinking.type=enabled with
// budget_tokens=31999, which the helper clamps to Kiro's 24576 maximum). When
// the failure occurs, the upstream still returns 200 but the model spends its
// budget on reasoning and emits only an empty / 1-character text_delta, which
// is a severe Claude Code UX regression. Routing those requests through Kiro's
// adaptive mode at a configurable effort level eliminates the failure mode in
// observed traffic while preserving meaningful reasoning depth.
//
// Behavior matrix:
//
//   - thinkingType == "" or "disabled"
//     → empty prefix (no thinking; matches the (none) suffix UX path).
//
//   - thinkingType == "adaptive"
//     → preserve client effort verbatim. An explicit
//     `claude-opus-4-6(high)` suffix from the user is always honored even when
//     defaultEffort is configured otherwise — the per-request opt-in wins.
//
//   - thinkingType == "enabled" && defaultEffort == "preserve"
//     → forward as enabled+budget unchanged. Operators whose Kiro account
//     does not exhibit the high-budget bug can opt back into the legacy path.
//
//   - thinkingType == "enabled" && defaultEffort in {"low","medium","high"}
//     → rewrite to adaptive at that effort level. Empty / unknown effort
//     coerces to KiroDefaultThinkingEffort ("medium").
//
//   - any other thinkingType
//     → empty prefix. The upstream defaults to no thinking, matching the
//     existing GenerateKiroThinkingPrefix behavior for unknown types.
//
// The function is deliberately pure (no I/O, no logging) so it can be
// exercised by unit tests without standing up a config.
func SelectKiroThinkingPrefix(thinkingType string, budgetTokens int, effort string, defaultEffort string) string {
	t := strings.ToLower(strings.TrimSpace(thinkingType))
	switch t {
	case "", "disabled":
		return ""
	case "adaptive":
		return GenerateKiroThinkingPrefix("adaptive", 0, effort)
	case "enabled":
		de := strings.ToLower(strings.TrimSpace(defaultEffort))
		if de == KiroThinkingEffortPreserve {
			return GenerateKiroThinkingPrefix("enabled", budgetTokens, "")
		}
		if de != "low" && de != "medium" && de != "high" {
			de = KiroDefaultThinkingEffort
		}
		return GenerateKiroThinkingPrefix("adaptive", 0, de)
	default:
		return ""
	}
}

func normalizeThinkingBudget(budget int) int {
	if budget <= 0 {
		budget = KiroDefaultBudgetTokens
	}
	if budget < KiroMinBudgetTokens {
		budget = KiroMinBudgetTokens
	}
	if budget > KiroMaxBudgetTokens {
		budget = KiroMaxBudgetTokens
	}
	return budget
}

// --- Kiro error classification ---
//
// These helpers mirror the minimum viable executor-local error policy described
// in the Kiro core-reliability spec (P0-2). They classify upstream HTTP and
// network errors into stable buckets that the conductor and telemetry layers
// can reason about without leaking secrets. A full multi-credential health
// model lives in `kiro.rs`; this is intentionally a smaller surface meant to
// be reusable by a shared provider-policy layer in the future.

// KiroErrorClass enumerates the runtime classifications for Kiro errors.
type KiroErrorClass string

const (
	// KiroErrUnauthorized indicates a 401 from the Kiro endpoint. The bearer
	// token is no longer accepted; the executor should attempt one bounded
	// force refresh and one retry before propagating.
	KiroErrUnauthorized KiroErrorClass = "unauthorized"
	// KiroErrQuotaExhausted indicates a 402 (quota/billing) response. The
	// credential is healthy but cannot serve more traffic in the current
	// window; the conductor should fail over without poisoning credentials.
	KiroErrQuotaExhausted KiroErrorClass = "quota_exhausted"
	// KiroErrForbidden indicates a 403 response that is not a refreshable
	// auth failure (e.g. policy / profile / region restriction).
	KiroErrForbidden KiroErrorClass = "forbidden"
	// KiroErrRateLimited indicates a 429 response. Treated as transient with
	// optional Retry-After backoff; the credential is not poisoned.
	KiroErrRateLimited KiroErrorClass = "rate_limited"
	// KiroErrServer indicates a transient upstream 5xx (or 408) failure. The
	// credential is not poisoned; the conductor may retry on another auth.
	KiroErrServer KiroErrorClass = "server"
	// KiroErrNetwork indicates a transport-level failure before a response
	// was received (DNS / TCP / TLS / EOF). The credential is not poisoned.
	KiroErrNetwork KiroErrorClass = "network"
	// KiroErrUnknown is used when the status code does not match any known
	// bucket and a network classification does not apply.
	KiroErrUnknown KiroErrorClass = "unknown"
	// KiroErrStreamRead indicates a streaming body read failed after the upstream
	// response was established. The conductor may fail over when no payload has
	// been sent yet.
	KiroErrStreamRead KiroErrorClass = "stream_read"
	// KiroErrStreamMalformed indicates the stream ended with an incomplete Kiro
	// JSON event buffered by the brace-count parser.
	KiroErrStreamMalformed KiroErrorClass = "stream_malformed"
)

// KiroError is a classified Kiro error returned by Execute / ExecuteStream.
// It implements the runtime executor's `StatusError` contract (`StatusCode()
// int`) and exposes an optional `RetryAfter()` for 429 handling, so the
// existing conductor failover and cooldown logic continues to work without
// further changes.
//
// The body is preserved for surfacing user-facing details and never includes
// secrets — callers must avoid logging or echoing it in contexts where token
// material could be present.
type KiroError struct {
	Class      KiroErrorClass
	Status     int
	Body       string
	retryAfter *time.Duration
	cause      error
}

// Error implements the error interface. The format intentionally avoids
// secrets and only surfaces classification, status, and the upstream body
// (which originates from Kiro and can contain operator-relevant details).
func (e *KiroError) Error() string {
	if e == nil {
		return ""
	}
	if e.Status > 0 {
		if e.Body != "" {
			return fmt.Sprintf("kiro: %s (status %d): %s", e.Class, e.Status, e.Body)
		}
		return fmt.Sprintf("kiro: %s (status %d)", e.Class, e.Status)
	}
	if e.cause != nil {
		return fmt.Sprintf("kiro: %s: %s", e.Class, e.cause.Error())
	}
	if e.Body != "" {
		return fmt.Sprintf("kiro: %s: %s", e.Class, e.Body)
	}
	return fmt.Sprintf("kiro: %s", e.Class)
}

// StatusCode satisfies the runtime executor `StatusError` interface so that
// the conductor's existing 401/402/403/429/5xx state-machine continues to
// work. A network error returns 0; the conductor treats that as a generic
// failure and proceeds to the next credential.
func (e *KiroError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// RetryAfter returns the upstream Retry-After hint for 429 responses, if
// present and parseable. The conductor uses it to schedule cooldowns.
func (e *KiroError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter == nil {
		return nil
	}
	d := *e.retryAfter
	return &d
}

// Unwrap exposes the underlying transport error for `errors.Is`/`errors.As`.
func (e *KiroError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Classification returns the stable error bucket for telemetry and tests.
func (e *KiroError) Classification() KiroErrorClass {
	if e == nil {
		return ""
	}
	return e.Class
}

// ClassifyKiroHTTPStatus classifies a non-2xx HTTP response from Kiro into
// the canonical KiroError. The body is captured verbatim for diagnostics; it
// must not contain credentials or refresh tokens (Kiro responses do not).
//
// The returned error is always non-nil for non-2xx statuses. For 2xx, callers
// must not invoke this helper. Caller-supplied `header` may be nil; it is only
// inspected to read `Retry-After` for 429.
func ClassifyKiroHTTPStatus(status int, body []byte, header http.Header) *KiroError {
	classified := &KiroError{
		Status: status,
		Body:   strings.TrimSpace(string(body)),
	}
	switch {
	case status == http.StatusUnauthorized: // 401
		classified.Class = KiroErrUnauthorized
	case status == http.StatusPaymentRequired: // 402
		classified.Class = KiroErrQuotaExhausted
	case status == http.StatusForbidden: // 403
		classified.Class = KiroErrForbidden
	case status == http.StatusTooManyRequests: // 429
		classified.Class = KiroErrRateLimited
		if d := parseRetryAfterHeader(header); d != nil {
			classified.retryAfter = d
		}
	case status == http.StatusRequestTimeout: // 408
		classified.Class = KiroErrServer
	case status >= 500 && status < 600:
		classified.Class = KiroErrServer
		if d := parseRetryAfterHeader(header); d != nil {
			classified.retryAfter = d
		} else if d := parseRetryAfterBody(body); d != nil {
			classified.retryAfter = d
		}
	default:
		classified.Class = KiroErrUnknown
	}
	return classified
}

// ClassifyKiroNetworkError wraps a transport-level error from
// `httpClient.Do(...)` (or comparable) into a KiroError with the network
// classification, unless the error is `context.Canceled` /
// `context.DeadlineExceeded`, in which case the original error is returned
// unchanged so that callers and tests see the canonical context error.
func ClassifyKiroNetworkError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &KiroError{
		Class: KiroErrNetwork,
		cause: err,
	}
}

// IsTransientNetworkError reports whether err is a network error that is
// safe to retry on a different credential without disabling the current one.
// It returns true for `KiroError{Class: KiroErrNetwork}` and for common
// `net.Error`/`io.EOF`/`syscall` style transport failures.
func IsTransientNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var ke *KiroError
	if errors.As(err, &ke) && ke != nil {
		return ke.Class == KiroErrNetwork || ke.Class == KiroErrServer || ke.Class == KiroErrRateLimited
	}
	var ne net.Error
	if errors.As(err, &ne) && ne != nil {
		return true
	}
	return false
}

// parseRetryAfterHeader parses the HTTP Retry-After header per RFC 9110
// (delta-seconds or HTTP-date). Returns nil if absent, malformed, or in the
// past.
func parseRetryAfterHeader(header http.Header) *time.Duration {
	if header == nil {
		return nil
	}
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return nil
		}
		d := time.Duration(seconds) * time.Second
		return &d
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return nil
		}
		return &d
	}
	return nil
}

func parseRetryAfterBody(body []byte) *time.Duration {
	var parsed interface{}
	if len(bytes.TrimSpace(body)) == 0 || json.Unmarshal(body, &parsed) != nil {
		return nil
	}
	seconds, ok := findRetryAfterSeconds(parsed)
	if !ok || seconds <= 0 {
		return nil
	}
	d := time.Duration(seconds) * time.Second
	return &d
}

func findRetryAfterSeconds(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		for key, child := range v {
			normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if normalized == "retry_after" || normalized == "retryafter" {
				if seconds, ok := retryAfterSecondsValue(child); ok {
					return seconds, true
				}
			}
			if seconds, ok := findRetryAfterSeconds(child); ok {
				return seconds, true
			}
		}
	case []interface{}:
		for _, child := range v {
			if seconds, ok := findRetryAfterSeconds(child); ok {
				return seconds, true
			}
		}
	}
	return 0, false
}

func retryAfterSecondsValue(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		seconds, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return seconds, err == nil
	default:
		return 0, false
	}
}

// SanitizeToolInput removes empty-string keys from a tool input map.
//
// Empty input (nil or zero-length) is replaced with `{}` so that the value can
// always be safely embedded in a JSON payload as a `json.RawMessage`. Without
// this fallback, marshaling a struct that contains an empty json.RawMessage
// fails with `json: error calling MarshalJSON for type json.RawMessage:
// unexpected end of JSON input`, which would block buildKiroAssistantHistoryMessage
// from serializing a prior assistant turn whose tool_use block did not carry an
// explicit `input` field. The empty-object placeholder matches the Anthropic
// Messages API contract for tool_use blocks (`input` is always an object).
func SanitizeToolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return json.RawMessage("{}")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(input, &m); err != nil {
		return input
	}
	changed := false
	for k := range m {
		if k == "" {
			delete(m, k)
			changed = true
		}
	}
	if !changed {
		return input
	}
	out, err := json.Marshal(m)
	if err != nil {
		return input
	}
	return out
}
