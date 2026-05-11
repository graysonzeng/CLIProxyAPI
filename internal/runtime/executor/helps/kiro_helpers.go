package helps

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Kiro protocol constants aligned with AIClient2API source.
// URL templates are var (not const) to allow test-time patching via httptest.
var (
	KiroSocialRefreshURLTemplate = "https://prod.{{region}}.auth.desktop.kiro.dev/refreshToken"
	KiroIDCRefreshURLTemplate    = "https://oidc.{{region}}.amazonaws.com/token"
	KiroBaseURLTemplate          = "https://q.{{region}}.amazonaws.com/generateAssistantResponse"
)

const (
	KiroVersion              = "0.11.63"
	KiroContentType          = "application/json"
	KiroAcceptJSON           = "application/json"
	KiroOriginAIEditor       = "AI_EDITOR"
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
