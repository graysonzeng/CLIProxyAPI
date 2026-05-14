package helps

import (
	"context"
	"crypto/sha256"
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
var KiroModelMapping = map[string]string{
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-opus-4-7":            "claude-opus-4.7",
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-sonnet-4-6":          "claude-sonnet-4.6",
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4-5-20251101":   "claude-opus-4.5",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
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
		"Connection":                  "close",
	}
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
	Type string // "content", "toolUse", "toolUseInput", "toolUseStop", "contextUsage"
	// Content holds the text for "content" events.
	Content string
	// ToolUse fields (populated for toolUse/toolUseInput/toolUseStop events).
	ToolName  string
	ToolUseID string
	ToolInput string
	ToolStop  bool
	// ContextUsage fields.
	ContextUsagePercentage float64
}

// ParseAwsEventStreamBuffer extracts JSON events from an AWS Event Stream buffer.
// It returns the parsed events and any remaining unparsed data.
// This mirrors AIClient2API's parseAwsEventStreamBuffer using brace-counting
// to handle nested JSON and binary headers.
func ParseAwsEventStreamBuffer(buffer string) (events []KiroStreamEvent, remaining string) {
	remaining = buffer
	searchStart := 0

	for {
		jsonStart := strings.Index(remaining[searchStart:], "{")
		if jsonStart < 0 {
			break
		}
		jsonStart += searchStart

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
			// Incomplete JSON — keep from jsonStart onward.
			remaining = remaining[jsonStart:]
			return events, remaining
		}

		jsonStr := remaining[jsonStart : jsonEnd+1]

		var parsed map[string]json.RawMessage
		if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
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
			remaining = ""
			return events, remaining
		}
	}

	// Trim consumed portion.
	if searchStart > 0 && len(remaining) > 0 {
		remaining = remaining[searchStart:]
	}
	return events, remaining
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
		var stop bool
		_ = json.Unmarshal(parsed["stop"], &stop)
		return &KiroStreamEvent{
			Type:     "toolUseStop",
			ToolStop: stop,
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

// SanitizeToolInput removes empty-string keys from a tool input map.
func SanitizeToolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return input
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
