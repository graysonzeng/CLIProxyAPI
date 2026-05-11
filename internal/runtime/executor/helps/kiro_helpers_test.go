package helps

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShortenKiroToolName_Short(t *testing.T) {
	name := "read_file"
	result := ShortenKiroToolName(name)
	if result != name {
		t.Errorf("expected %q, got %q", name, result)
	}
}

func TestShortenKiroToolName_Long(t *testing.T) {
	name := strings.Repeat("a", 100)
	result := ShortenKiroToolName(name)
	if len(result) > KiroMaxToolNameLength {
		t.Errorf("expected len <= %d, got %d", KiroMaxToolNameLength, len(result))
	}
	if !strings.Contains(result, "_") {
		t.Error("expected hash suffix with underscore separator")
	}
}

func TestShortenKiroToolName_ExactLength(t *testing.T) {
	name := strings.Repeat("x", KiroMaxToolNameLength)
	result := ShortenKiroToolName(name)
	if result != name {
		t.Errorf("expected name unchanged at exact max length")
	}
}

func TestBuildKiroToolNameMaps(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"name": "short_tool", "description": "d"}`),
		json.RawMessage(`{"name": "` + strings.Repeat("long", 30) + `", "description": "d"}`),
	}
	maps := BuildKiroToolNameMaps(tools)

	// Short tool should be identity-mapped.
	if maps.ToKiroName("short_tool") != "short_tool" {
		t.Errorf("short tool name should not be shortened")
	}

	longName := strings.Repeat("long", 30)
	kiroName := maps.ToKiroName(longName)
	if len(kiroName) > KiroMaxToolNameLength {
		t.Errorf("kiro name too long: %d", len(kiroName))
	}

	// Reverse mapping.
	restored := maps.FromKiroName(kiroName)
	if restored != longName {
		t.Errorf("expected restored name %q, got %q", longName, restored)
	}
}

func TestBuildKiroToolNameMaps_Nil(t *testing.T) {
	var maps *KiroToolNameMaps
	// Should not panic.
	if maps.ToKiroName("foo") != "foo" {
		t.Error("nil maps ToKiroName should return original")
	}
	if maps.FromKiroName("foo") != "foo" {
		t.Error("nil maps FromKiroName should return original")
	}
}

func TestGenerateKiroMachineID(t *testing.T) {
	id1 := GenerateKiroMachineID("uuid-1", "", "")
	id2 := GenerateKiroMachineID("uuid-2", "", "")
	if id1 == id2 {
		t.Error("different UUIDs should produce different machine IDs")
	}
	if len(id1) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got len=%d", len(id1))
	}

	// Fallback to profileArn.
	id3 := GenerateKiroMachineID("", "arn:123", "")
	id4 := GenerateKiroMachineID("", "arn:456", "")
	if id3 == id4 {
		t.Error("different profileArns should produce different machine IDs")
	}

	// Default fallback.
	idDefault := GenerateKiroMachineID("", "", "")
	if idDefault == "" {
		t.Error("default machine ID should not be empty")
	}
}

func TestMapKiroModel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"claude-sonnet-4-5", "claude-sonnet-4.5"},
		{"claude-haiku-4-5", "claude-haiku-4.5"},
		{"claude-opus-4-7", "claude-opus-4.7"},
		{"unknown-model", "unknown-model"},
	}
	for _, tt := range tests {
		result := MapKiroModel(tt.input)
		if result != tt.expected {
			t.Errorf("MapKiroModel(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestKiroRequestHeaders(t *testing.T) {
	headers := KiroRequestHeaders("abc123")
	if headers["Content-Type"] != KiroContentType {
		t.Errorf("expected Content-Type %q", KiroContentType)
	}
	if !strings.Contains(headers["user-agent"], "KiroIDE") {
		t.Error("user-agent should contain KiroIDE")
	}
	if !strings.Contains(headers["user-agent"], "abc123") {
		t.Error("user-agent should contain machine ID")
	}
	if headers["Connection"] != "close" {
		t.Error("Connection header should be 'close'")
	}
}

func TestKiroPerRequestHeaders(t *testing.T) {
	headers := KiroPerRequestHeaders("mytoken")
	if headers["Authorization"] != "Bearer mytoken" {
		t.Errorf("unexpected Authorization: %q", headers["Authorization"])
	}
	if headers["amz-sdk-invocation-id"] == "" {
		t.Error("amz-sdk-invocation-id should not be empty")
	}
}

func TestKiroRefreshURLs(t *testing.T) {
	socialURL := KiroSocialRefreshURL("us-east-1")
	if !strings.Contains(socialURL, "us-east-1") || !strings.Contains(socialURL, "kiro.dev") {
		t.Errorf("unexpected social URL: %s", socialURL)
	}

	idcURL := KiroIDCRefreshURL("us-east-1")
	if !strings.Contains(idcURL, "oidc.us-east-1.amazonaws.com") {
		t.Errorf("unexpected IDC URL: %s", idcURL)
	}

	baseURL := KiroBaseURL("")
	if !strings.Contains(baseURL, "us-east-1") {
		t.Error("empty region should default to us-east-1")
	}
}

func TestParseAwsEventStreamBuffer_ContentEvent(t *testing.T) {
	buffer := `some binary header{"content": "Hello world"}more binary`
	events, remaining := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "content" {
		t.Errorf("expected content event, got %s", events[0].Type)
	}
	if events[0].Content != "Hello world" {
		t.Errorf("unexpected content: %q", events[0].Content)
	}
	_ = remaining
}

func TestParseAwsEventStreamBuffer_ToolUseEvent(t *testing.T) {
	buffer := `{"name": "read_file", "toolUseId": "tu-123", "input": "{\"path\":\"/foo\"}", "stop": true}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "toolUse" {
		t.Errorf("expected toolUse, got %s", events[0].Type)
	}
	if events[0].ToolName != "read_file" {
		t.Errorf("unexpected tool name: %q", events[0].ToolName)
	}
	if events[0].ToolUseID != "tu-123" {
		t.Errorf("unexpected toolUseId: %q", events[0].ToolUseID)
	}
	if !events[0].ToolStop {
		t.Error("expected stop=true")
	}
}

func TestParseAwsEventStreamBuffer_ToolUseInputEvent(t *testing.T) {
	buffer := `{"input": "partial json", "toolUseId": "tu-456"}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "toolUseInput" {
		t.Errorf("expected toolUseInput, got %s", events[0].Type)
	}
	if events[0].ToolInput != "partial json" {
		t.Errorf("unexpected input: %q", events[0].ToolInput)
	}
}

func TestParseAwsEventStreamBuffer_ContextUsageEvent(t *testing.T) {
	buffer := `{"contextUsagePercentage": 42.5}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "contextUsage" {
		t.Errorf("expected contextUsage, got %s", events[0].Type)
	}
	if events[0].ContextUsagePercentage != 42.5 {
		t.Errorf("unexpected percentage: %f", events[0].ContextUsagePercentage)
	}
}

func TestParseAwsEventStreamBuffer_MultipleEvents(t *testing.T) {
	buffer := `binary{"content": "A"}binary{"content": "B"}trail`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Content != "A" || events[1].Content != "B" {
		t.Errorf("unexpected contents: %q, %q", events[0].Content, events[1].Content)
	}
}

func TestParseAwsEventStreamBuffer_IncompleteJSON(t *testing.T) {
	buffer := `binary{"content": "partial`
	events, remaining := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 0 {
		t.Errorf("expected 0 events for incomplete JSON, got %d", len(events))
	}
	if !strings.Contains(remaining, `{"content": "partial`) {
		t.Errorf("remaining should contain incomplete JSON, got %q", remaining)
	}
}

func TestParseAwsEventStreamBuffer_NestedJSON(t *testing.T) {
	buffer := `{"content": "text with {nested} braces"}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Content != "text with {nested} braces" {
		t.Errorf("unexpected content: %q", events[0].Content)
	}
}

func TestParseAwsEventStreamBuffer_FollowupPromptSkipped(t *testing.T) {
	buffer := `{"content": "hi", "followupPrompt": "ask me"}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 0 {
		t.Errorf("followupPrompt events should be skipped, got %d", len(events))
	}
}

func TestParseAwsEventStreamBuffer_ToolUseStopEvent(t *testing.T) {
	buffer := `{"stop": true}`
	events, _ := ParseAwsEventStreamBuffer(buffer)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "toolUseStop" {
		t.Errorf("expected toolUseStop, got %s", events[0].Type)
	}
	if !events[0].ToolStop {
		t.Error("expected stop=true")
	}
}

func TestGenerateKiroThinkingPrefix_Enabled(t *testing.T) {
	prefix := GenerateKiroThinkingPrefix("enabled", 10000, "")
	if !strings.Contains(prefix, "enabled</thinking_mode>") {
		t.Error("expected enabled mode tag")
	}
	if !strings.Contains(prefix, "10000</max_thinking_length>") {
		t.Error("expected budget 10000")
	}
}

func TestGenerateKiroThinkingPrefix_Adaptive(t *testing.T) {
	prefix := GenerateKiroThinkingPrefix("adaptive", 0, "medium")
	if !strings.Contains(prefix, "adaptive</thinking_mode>") {
		t.Error("expected adaptive mode tag")
	}
	if !strings.Contains(prefix, "medium</thinking_effort>") {
		t.Error("expected medium effort")
	}
}

func TestGenerateKiroThinkingPrefix_AdaptiveDefaultEffort(t *testing.T) {
	prefix := GenerateKiroThinkingPrefix("adaptive", 0, "invalid")
	if !strings.Contains(prefix, "high</thinking_effort>") {
		t.Errorf("expected default high effort, got %q", prefix)
	}
}

func TestGenerateKiroThinkingPrefix_Disabled(t *testing.T) {
	prefix := GenerateKiroThinkingPrefix("disabled", 0, "")
	if prefix != "" {
		t.Errorf("expected empty prefix for disabled, got %q", prefix)
	}
}

func TestSanitizeToolInput(t *testing.T) {
	input := json.RawMessage(`{"":"bad", "good":"value"}`)
	result := SanitizeToolInput(input)
	var m map[string]interface{}
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m[""]; ok {
		t.Error("empty key should be removed")
	}
	if m["good"] != "value" {
		t.Error("good key should be preserved")
	}
}

func TestSanitizeToolInput_NoChange(t *testing.T) {
	input := json.RawMessage(`{"a":"1","b":"2"}`)
	result := SanitizeToolInput(input)
	if string(result) != string(input) {
		t.Errorf("unchanged input should be returned as-is")
	}
}
