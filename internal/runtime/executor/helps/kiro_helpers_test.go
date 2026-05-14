package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestKiroUsageLimitsURL(t *testing.T) {
	tests := []struct {
		name              string
		region            string
		profileArn        string
		wantHost          string
		wantProfileArn    string
		wantProfileArnSet bool
	}{
		{
			name:              "profile arn",
			region:            "us-west-2",
			profileArn:        "arn:aws:kiro:profile",
			wantHost:          "q.us-west-2.amazonaws.com",
			wantProfileArn:    "arn:aws:kiro:profile",
			wantProfileArnSet: true,
		},
		{
			name:              "empty region and profile arn",
			region:            "",
			profileArn:        "",
			wantHost:          "q.us-east-1.amazonaws.com",
			wantProfileArnSet: false,
		},
		{
			name:              "empty profile arn",
			region:            "us-west-2",
			profileArn:        "",
			wantHost:          "q.us-west-2.amazonaws.com",
			wantProfileArnSet: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := KiroUsageLimitsURL(tt.region, tt.profileArn)
			parsed, err := url.Parse(got)
			if err != nil {
				t.Fatalf("url.Parse returned error: %v", err)
			}
			if parsed.Scheme != "https" || parsed.Host != tt.wantHost || parsed.Path != "/getUsageLimits" {
				t.Fatalf("unexpected URL = %s", got)
			}
			query := parsed.Query()
			if query.Get("origin") != KiroOriginAIEditor {
				t.Fatalf("origin = %q, want %q", query.Get("origin"), KiroOriginAIEditor)
			}
			if query.Get("resourceType") != KiroResourceAgentic {
				t.Fatalf("resourceType = %q, want %q", query.Get("resourceType"), KiroResourceAgentic)
			}
			if query.Get("isEmailRequired") != "true" {
				t.Fatalf("isEmailRequired = %q, want true", query.Get("isEmailRequired"))
			}
			if gotProfileArn, ok := query["profileArn"]; ok != tt.wantProfileArnSet {
				t.Fatalf("profileArn present = %v, want %v; values = %v", ok, tt.wantProfileArnSet, gotProfileArn)
			}
			if tt.wantProfileArnSet && query.Get("profileArn") != tt.wantProfileArn {
				t.Fatalf("profileArn = %q, want %q", query.Get("profileArn"), tt.wantProfileArn)
			}
		})
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

// --- Kiro error classification tests ---

func TestClassifyKiroHTTPStatus_KnownBuckets(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		header    http.Header
		wantClass KiroErrorClass
	}{
		{name: "401 unauthorized", status: 401, body: "expired token", wantClass: KiroErrUnauthorized},
		{name: "402 quota exhausted", status: 402, body: "monthly limit reached", wantClass: KiroErrQuotaExhausted},
		{name: "403 forbidden policy", status: 403, body: "profile policy denied", wantClass: KiroErrForbidden},
		{name: "408 request timeout treated as transient server", status: 408, wantClass: KiroErrServer},
		{name: "429 rate limited", status: 429, body: "too many requests", wantClass: KiroErrRateLimited},
		{name: "500 internal server", status: 500, wantClass: KiroErrServer},
		{name: "502 bad gateway", status: 502, wantClass: KiroErrServer},
		{name: "503 service unavailable", status: 503, wantClass: KiroErrServer},
		{name: "504 gateway timeout", status: 504, wantClass: KiroErrServer},
		{name: "420 unknown 4xx", status: 420, wantClass: KiroErrUnknown},
		{name: "418 teapot unknown", status: 418, wantClass: KiroErrUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ke := ClassifyKiroHTTPStatus(tt.status, []byte(tt.body), tt.header)
			if ke == nil {
				t.Fatalf("ClassifyKiroHTTPStatus returned nil")
			}
			if ke.Classification() != tt.wantClass {
				t.Fatalf("Classification = %q, want %q", ke.Classification(), tt.wantClass)
			}
			if ke.StatusCode() != tt.status {
				t.Fatalf("StatusCode = %d, want %d", ke.StatusCode(), tt.status)
			}
			// Error string must include classification and status code, but
			// must not leak headers (no token material is supplied to this
			// helper, but the format must remain stable).
			msg := ke.Error()
			if msg == "" {
				t.Fatalf("Error() returned empty string")
			}
			if !strings.Contains(msg, string(tt.wantClass)) {
				t.Fatalf("Error() = %q, expected to contain class %q", msg, tt.wantClass)
			}
		})
	}
}

func TestClassifyKiroHTTPStatus_RetryAfterSeconds(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "12")
	ke := ClassifyKiroHTTPStatus(429, []byte("rate limited"), header)
	if ke.Classification() != KiroErrRateLimited {
		t.Fatalf("Classification = %q, want %q", ke.Classification(), KiroErrRateLimited)
	}
	got := ke.RetryAfter()
	if got == nil {
		t.Fatalf("RetryAfter = nil, want 12s")
	}
	if *got != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", *got)
	}
}

func TestClassifyKiroHTTPStatus_RetryAfterDate(t *testing.T) {
	header := http.Header{}
	future := time.Now().UTC().Add(45 * time.Second).Format(http.TimeFormat)
	header.Set("Retry-After", future)
	ke := ClassifyKiroHTTPStatus(429, nil, header)
	got := ke.RetryAfter()
	if got == nil {
		t.Fatalf("RetryAfter = nil, want positive duration")
	}
	if *got <= 0 {
		t.Fatalf("RetryAfter = %v, want >0", *got)
	}
}

func TestClassifyKiroHTTPStatus_RetryAfterPastDateIgnored(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", time.Now().UTC().Add(-1*time.Hour).Format(http.TimeFormat))
	ke := ClassifyKiroHTTPStatus(429, nil, header)
	if ke.RetryAfter() != nil {
		t.Fatalf("RetryAfter for past date should be ignored")
	}
}

func TestClassifyKiroHTTPStatus_NonRateLimitedIgnoresRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "30")
	ke := ClassifyKiroHTTPStatus(503, nil, header)
	if ke.Classification() != KiroErrServer {
		t.Fatalf("Classification = %q, want %q", ke.Classification(), KiroErrServer)
	}
	// 5xx classifier intentionally does not surface Retry-After today;
	// document the boundary so future change is intentional.
	if ke.RetryAfter() != nil {
		t.Fatalf("non-429 should not expose Retry-After in current contract; got %v", *ke.RetryAfter())
	}
}

func TestClassifyKiroNetworkError_WrapsTransport(t *testing.T) {
	src := errors.New("dial tcp: i/o timeout")
	classified := ClassifyKiroNetworkError(src)
	var ke *KiroError
	if !errors.As(classified, &ke) {
		t.Fatalf("expected *KiroError, got %T: %v", classified, classified)
	}
	if ke.Classification() != KiroErrNetwork {
		t.Fatalf("Classification = %q, want %q", ke.Classification(), KiroErrNetwork)
	}
	if ke.StatusCode() != 0 {
		t.Fatalf("StatusCode = %d, want 0 for network error", ke.StatusCode())
	}
	if !errors.Is(classified, src) {
		t.Fatalf("expected wrapped error to satisfy errors.Is(src)")
	}
}

func TestClassifyKiroNetworkError_PreservesContextErrors(t *testing.T) {
	if got := ClassifyKiroNetworkError(context.Canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("context.Canceled should be returned unchanged, got %v", got)
	}
	if got := ClassifyKiroNetworkError(context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("context.DeadlineExceeded should be returned unchanged, got %v", got)
	}
}

func TestKiroError_NilSafety(t *testing.T) {
	var ke *KiroError
	if got := ke.Error(); got != "" {
		t.Fatalf("nil KiroError Error() = %q, want empty", got)
	}
	if got := ke.StatusCode(); got != 0 {
		t.Fatalf("nil KiroError StatusCode() = %d, want 0", got)
	}
	if ke.RetryAfter() != nil {
		t.Fatalf("nil KiroError RetryAfter() should be nil")
	}
	if got := ke.Classification(); got != "" {
		t.Fatalf("nil KiroError Classification() = %q, want empty", got)
	}
	if ke.Unwrap() != nil {
		t.Fatalf("nil KiroError Unwrap() should be nil")
	}
}

func TestKiroError_DoesNotLeakSecrets(t *testing.T) {
	// The body is preserved verbatim; verify that the error format itself
	// does not echo arbitrary header material the caller never supplied.
	ke := ClassifyKiroHTTPStatus(401, []byte(`{"error":"invalid bearer"}`), http.Header{})
	msg := ke.Error()
	if strings.Contains(msg, "Bearer") {
		t.Fatalf("Error() must not surface the literal Authorization header value: %q", msg)
	}
	// Provide a body that contains a fake token-shaped string and verify it
	// is NOT silently scrubbed (we explicitly preserve upstream body), but
	// that the helper does not append any other secret-like fields.
	bodyWithMarker := fmt.Sprintf("upstream-body-%d", time.Now().UnixNano())
	ke2 := ClassifyKiroHTTPStatus(403, []byte(bodyWithMarker), nil)
	if !strings.Contains(ke2.Error(), bodyWithMarker) {
		t.Fatalf("upstream body should be preserved in Error(): %q", ke2.Error())
	}
}
