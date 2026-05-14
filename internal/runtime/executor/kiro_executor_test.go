package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"

	// Register protocol translators for executor translation regression tests.
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	// Register Claude thinking provider applier (needed by ApplyThinking tests).
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
)

type kiroUsageCapture struct {
	ch chan usage.Record
}

func (c *kiroUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider != "kiro" {
		return
	}
	select {
	case c.ch <- record:
	default:
	}
}

func registerKiroUsageCapture() *kiroUsageCapture {
	capture := &kiroUsageCapture{ch: make(chan usage.Record, 16)}
	usage.RegisterPlugin(capture)
	return capture
}

func waitForKiroUsageRecord(t *testing.T, capture *kiroUsageCapture, authID string) usage.Record {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case record := <-capture.ch:
			if record.AuthID == authID {
				return record
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for Kiro usage record for auth %q", authID)
		}
	}
}

func TestExtractThinkingFromText(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantThinking  string
		wantRemaining string
	}{
		{
			name:          "no thinking tags",
			input:         "Hello world",
			wantThinking:  "",
			wantRemaining: "Hello world",
		},
		{
			name:          "thinking tags with content",
			input:         "<thinking>\nI need to think\n</thinking>\n\nThe answer is 42",
			wantThinking:  "I need to think\n",
			wantRemaining: "The answer is 42",
		},
		{
			name:          "thinking only",
			input:         "<thinking>just thinking</thinking>",
			wantThinking:  "just thinking",
			wantRemaining: "",
		},
		{
			name:          "text before thinking",
			input:         "prefix <thinking>deep thought</thinking>\n\nafter",
			wantThinking:  "deep thought",
			wantRemaining: "prefix after",
		},
		{
			name:          "unclosed thinking",
			input:         "<thinking>incomplete",
			wantThinking:  "incomplete",
			wantRemaining: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thinking, remaining := extractThinkingFromText(tt.input)
			if thinking != tt.wantThinking {
				t.Errorf("thinking = %q, want %q", thinking, tt.wantThinking)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestBuildClaudeMessageJSON_TextOnly(t *testing.T) {
	raw := `binary{"content": "Hello"}binary{"content": " world"}`
	result := buildClaudeMessageJSON([]byte(raw), nil, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if msg["type"] != "message" {
		t.Errorf("expected type message, got %v", msg["type"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("expected role assistant, got %v", msg["role"])
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %v", msg["stop_reason"])
	}

	content, ok := msg["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatal("expected content blocks")
	}

	block := content[0].(map[string]interface{})
	if block["type"] != "text" {
		t.Errorf("expected text block, got %v", block["type"])
	}
	text := block["text"].(string)
	if text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", text)
	}
}

func TestBuildClaudeMessageSSE_TranslatesToOpenAINonStream(t *testing.T) {
	raw := `binary{"content": "Hello"}binary{"content": " world"}`
	claudeSSE := buildClaudeMessageSSE([]byte(raw), nil, "claude-sonnet-4-5")

	var param any
	result := sdktranslator.TranslateNonStream(
		context.Background(),
		sdktranslator.FromString("claude"),
		sdktranslator.FromString("openai"),
		"claude-sonnet-4-5",
		nil,
		nil,
		claudeSSE,
		&param,
	)

	if got := gjson.GetBytes(result, "choices.0.message.content").String(); got != "Hello world" {
		t.Fatalf("OpenAI content = %q, want %q; raw=%s", got, "Hello world", string(result))
	}
}

func TestBuildClaudeMessageJSON_WithToolUse(t *testing.T) {
	raw := `{"content": "Let me read that."}binary{"name": "read_file", "toolUseId": "tu-1", "input": "{\"path\":\"/test\"}", "stop": true}`
	maps := helps.BuildKiroToolNameMaps(nil)
	result := buildClaudeMessageJSON([]byte(raw), maps, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if msg["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %v", msg["stop_reason"])
	}

	content := msg["content"].([]interface{})
	if len(content) < 2 {
		t.Fatalf("expected at least 2 content blocks, got %d", len(content))
	}

	toolBlock := content[len(content)-1].(map[string]interface{})
	if toolBlock["type"] != "tool_use" {
		t.Errorf("expected tool_use block, got %v", toolBlock["type"])
	}
	if toolBlock["name"] != "read_file" {
		t.Errorf("expected tool name read_file, got %v", toolBlock["name"])
	}
}

func TestBuildClaudeMessageJSON_WithThinking(t *testing.T) {
	raw := `{"content": "<thinking>\nLet me think.\n</thinking>\n\nThe answer."}`
	result := buildClaudeMessageJSON([]byte(raw), nil, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	content := msg["content"].([]interface{})
	hasThinking := false
	hasText := false
	for _, block := range content {
		b := block.(map[string]interface{})
		if b["type"] == "thinking" {
			hasThinking = true
		}
		if b["type"] == "text" {
			hasText = true
		}
	}
	if !hasThinking {
		t.Error("expected thinking content block")
	}
	if !hasText {
		t.Error("expected text content block")
	}
}

func TestBuildClaudeMessageJSON_DuplicateContentDedup(t *testing.T) {
	// Kiro sometimes sends duplicate content events.
	raw := `{"content": "Hello"}garbage{"content": "Hello"}garbage{"content": " world"}`
	result := buildClaudeMessageJSON([]byte(raw), nil, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	content := msg["content"].([]interface{})
	for _, block := range content {
		b := block.(map[string]interface{})
		if b["type"] == "text" {
			text := b["text"].(string)
			// Should be "Hello world" not "HelloHello world".
			if text != "Hello world" {
				t.Errorf("expected deduped content 'Hello world', got %q", text)
			}
		}
	}
}

func TestStreamKiroToClaudeSSE_TextOnly(t *testing.T) {
	raw := `binary{"content": "Hello"}binary{"content": " world"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})

	if len(lines) == 0 {
		t.Fatal("expected SSE lines")
	}

	// Check for message_start
	hasMessageStart := false
	hasMessageStop := false
	hasContentDelta := false
	for _, line := range lines {
		s := string(line)
		if strings.Contains(s, "message_start") {
			hasMessageStart = true
		}
		if strings.Contains(s, "message_stop") {
			hasMessageStop = true
		}
		if strings.Contains(s, "text_delta") {
			hasContentDelta = true
		}
	}
	if !hasMessageStart {
		t.Error("expected message_start event")
	}
	if !hasMessageStop {
		t.Error("expected message_stop event")
	}
	if !hasContentDelta {
		t.Error("expected text_delta event")
	}
}

func TestStreamKiroToClaudeSSE_TranslatesToOpenAIChunks(t *testing.T) {
	raw := `binary{"content": "Hello"}binary{"content": " world"}`
	reader := strings.NewReader(raw)
	var param any
	var content strings.Builder
	var chunkCount int

	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		for _, dataLine := range claudeSSEDataLines(line) {
			chunks := sdktranslator.TranslateStream(
				context.Background(),
				sdktranslator.FromString("claude"),
				sdktranslator.FromString("openai"),
				"claude-sonnet-4-5",
				nil,
				nil,
				dataLine,
				&param,
			)
			for _, chunk := range chunks {
				chunkCount++
				content.WriteString(gjson.GetBytes(chunk, "choices.0.delta.content").String())
			}
		}
	})

	if chunkCount == 0 {
		t.Fatal("expected translated OpenAI chunks")
	}
	if got := content.String(); got != "Hello world" {
		t.Fatalf("OpenAI stream content = %q, want %q", got, "Hello world")
	}
}

func TestStreamKiroToClaudeSSE_WithToolUse(t *testing.T) {
	raw := `{"content": "text"}binary{"name": "bash", "toolUseId": "tu-1", "stop": false}binary{"input": "{\"cmd\":\"ls\"}"}binary{"stop": true}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})

	hasToolStart := false
	hasInputDelta := false
	hasToolStop := false
	for _, line := range lines {
		s := string(line)
		if strings.Contains(s, `"tool_use"`) && strings.Contains(s, "content_block_start") {
			hasToolStart = true
		}
		if strings.Contains(s, "input_json_delta") {
			hasInputDelta = true
		}
		if strings.Contains(s, "content_block_stop") {
			hasToolStop = true
		}
	}
	if !hasToolStart {
		t.Error("expected tool_use content_block_start")
	}
	if !hasInputDelta {
		t.Error("expected input_json_delta")
	}
	if !hasToolStop {
		t.Error("expected content_block_stop for tool")
	}
}

// chunkedReader delivers data in small pieces to simulate a streaming HTTP response.
type chunkedReader struct {
	chunks []string
	idx    int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestStreamKiroToClaudeSSE_Incremental(t *testing.T) {
	// Deliver two content events in separate chunks to verify incremental emission.
	reader := &chunkedReader{
		chunks: []string{
			`binary{"content": "Hello"}`,
			`binary{"content": " world"}`,
		},
	}

	var emitCount int
	var emittedAfterChunk1 int
	origIdx := &reader.idx

	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		emitCount++
		// After the reader has consumed only the first chunk (idx==1),
		// we should already have received SSE lines (message_start + first content events).
		if *origIdx <= 1 {
			emittedAfterChunk1 = emitCount
		}
	})

	if emitCount == 0 {
		t.Fatal("expected SSE lines to be emitted")
	}
	// message_start is emitted before any read, so at minimum 1 line should
	// have been emitted before all chunks are consumed.
	if emittedAfterChunk1 == 0 {
		t.Error("expected SSE lines to be emitted incrementally before all data is read")
	}
	// The first chunk should produce: message_start + content_block_start + content_block_delta = 3 lines.
	// If all data were read first (old behavior), emittedAfterChunk1 would be 0.
	if emittedAfterChunk1 < 2 {
		t.Errorf("expected at least 2 SSE lines after first chunk, got %d", emittedAfterChunk1)
	}
}

func TestKiroExecutorIdentifier(t *testing.T) {
	e := NewKiroExecutor(nil)
	if e.Identifier() != "kiro" {
		t.Errorf("expected identifier 'kiro', got %q", e.Identifier())
	}
}

func TestKiroExecutorExecutePublishesSuccessUsage(t *testing.T) {
	capture := registerKiroUsageCapture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-test" {
			t.Fatalf("Authorization = %q, want Bearer at-test", got)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"ok"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	authID := "kiro-usage-nonstream"
	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-test",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "ok" {
		t.Fatalf("Execute content = %q, want ok; payload=%s", got, string(resp.Payload))
	}

	record := waitForKiroUsageRecord(t, capture, authID)
	if record.Failed {
		t.Fatalf("usage record Failed = true, want false: %+v", record.Fail)
	}
	if record.Provider != "kiro" || record.Model != "claude-sonnet-4-5" {
		t.Fatalf("usage record provider/model = %s/%s, want kiro/claude-sonnet-4-5", record.Provider, record.Model)
	}
}

func TestKiroExecutorExecuteStreamPublishesSuccessUsage(t *testing.T) {
	capture := registerKiroUsageCapture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-stream" {
			t.Fatalf("Authorization = %q, want Bearer at-stream", got)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"stream ok"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	authID := "kiro-usage-stream"
	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var sawContent bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "stream ok") {
			sawContent = true
		}
	}
	if !sawContent {
		t.Fatal("expected stream content chunk containing stream ok")
	}

	record := waitForKiroUsageRecord(t, capture, authID)
	if record.Failed {
		t.Fatalf("usage record Failed = true, want false: %+v", record.Fail)
	}
	if record.Provider != "kiro" || record.Model != "claude-sonnet-4-5" {
		t.Fatalf("usage record provider/model = %s/%s, want kiro/claude-sonnet-4-5", record.Provider, record.Model)
	}
}

// TestKiroThinkingPipelineIntegration verifies that thinking.ApplyThinking()
// correctly processes thinking configuration before buildKiroCodeWhispererRequest
// consumes it. This covers the HIGH-3 fix: Kiro executor must go through the
// central thinking pipeline for suffix override, capability validation, and
// budget normalization.
func TestKiroThinkingPipelineIntegration(t *testing.T) {
	tests := []struct {
		name               string
		model              string
		body               string
		wantThinkingType   string
		wantBudgetTokens   int
		wantMaxBudget      int // if >0, assert budget_tokens <= this value
		wantThinkingPrefix bool
	}{
		{
			name:               "suffix budget override",
			model:              "claude-sonnet-4-5(8192)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "enabled",
			wantBudgetTokens:   8192,
			wantThinkingPrefix: true,
		},
		{
			name:               "suffix none disables thinking",
			model:              "claude-sonnet-4-5(none)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantThinkingType:   "disabled",
			wantBudgetTokens:   0,
			wantThinkingPrefix: false,
		},
		{
			name:               "body thinking config passthrough",
			model:              "claude-sonnet-4-5",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantThinkingType:   "enabled",
			wantBudgetTokens:   4096,
			wantThinkingPrefix: true,
		},
		{
			name:               "no thinking config passthrough",
			model:              "claude-sonnet-4-5",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "",
			wantThinkingPrefix: false,
		},
		{
			name:               "budget clamped to model max",
			model:              "claude-sonnet-4-5(99999)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "enabled",
			wantMaxBudget:      24576,
			wantThinkingPrefix: true,
		},
		{
			name:               "suffix auto enables thinking",
			model:              "claude-sonnet-4-5(auto)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "enabled",
			wantThinkingPrefix: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			processed, err := thinking.ApplyThinking(body, tt.model, "claude", "claude", "kiro")
			if err != nil {
				t.Fatalf("ApplyThinking error: %v", err)
			}

			thinkingType := gjson.GetBytes(processed, "thinking.type").String()
			budgetTokens := int(gjson.GetBytes(processed, "thinking.budget_tokens").Int())

			if tt.wantThinkingType != "" && thinkingType != tt.wantThinkingType {
				t.Errorf("thinking.type = %q, want %q", thinkingType, tt.wantThinkingType)
			}
			if tt.wantBudgetTokens > 0 && budgetTokens != tt.wantBudgetTokens {
				t.Errorf("thinking.budget_tokens = %d, want %d", budgetTokens, tt.wantBudgetTokens)
			}
			if tt.wantMaxBudget > 0 && budgetTokens > tt.wantMaxBudget {
				t.Errorf("thinking.budget_tokens = %d, want <= %d", budgetTokens, tt.wantMaxBudget)
			}

			// Verify buildKiroCodeWhispererRequest consumes the processed thinking.
			cwReq, _, errBuild := buildKiroCodeWhispererRequest(processed, nil)
			if errBuild != nil {
				t.Fatalf("buildKiroCodeWhispererRequest error: %v", errBuild)
			}

			cwStr := string(cwReq)
			hasThinkingPrefix := strings.Contains(cwStr, "thinking_mode")
			if tt.wantThinkingPrefix && !hasThinkingPrefix {
				t.Error("expected thinking prefix in CodeWhisperer request, not found")
			}
			if !tt.wantThinkingPrefix && hasThinkingPrefix {
				t.Error("unexpected thinking prefix in CodeWhisperer request")
			}
		})
	}
}

// --- Tool use regression tests (MEDIUM-2a) ---

// TestBuildClaudeMessageJSON_SameToolUseID_MultiEvent verifies that multiple
// toolUse events sharing the same toolUseID are merged into a single tool_use
// content block with concatenated input. This is the real Kiro regression
// scenario where input is split across chunks and each chunk carries `name`.
func TestBuildClaudeMessageJSON_SameToolUseID_MultiEvent(t *testing.T) {
	// Simulate two toolUse events with the same toolUseId, each carrying a
	// partial input fragment. The second event carries stop=true.
	raw := `{"content": "Checking weather."}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "{\"location\":", "stop": false}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "\"Tokyo\"}", "stop": true}`
	maps := helps.BuildKiroToolNameMaps(nil)
	result := buildClaudeMessageJSON([]byte(raw), maps, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if msg["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %v", msg["stop_reason"])
	}

	content := msg["content"].([]interface{})
	// Count tool_use blocks — should be exactly 1.
	var toolBlocks []map[string]interface{}
	for _, block := range content {
		b := block.(map[string]interface{})
		if b["type"] == "tool_use" {
			toolBlocks = append(toolBlocks, b)
		}
	}
	if len(toolBlocks) != 1 {
		t.Fatalf("expected exactly 1 tool_use block, got %d", len(toolBlocks))
	}

	tb := toolBlocks[0]
	if tb["id"] != "tu-dup" {
		t.Errorf("expected tool_use id tu-dup, got %v", tb["id"])
	}
	if tb["name"] != "get_weather" {
		t.Errorf("expected tool name get_weather, got %v", tb["name"])
	}
	// Verify the input is the concatenation of both fragments parsed as JSON.
	inputMap, ok := tb["input"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected input to be a JSON object, got %T: %v", tb["input"], tb["input"])
	}
	if inputMap["location"] != "Tokyo" {
		t.Errorf("expected input.location=Tokyo, got %v", inputMap["location"])
	}
}

// TestStreamKiroToClaudeSSE_SameToolUseID_MultiEvent verifies that multiple
// toolUse events with the same toolUseID emit only one content_block_start,
// multiple input_json_delta events preserving the original shards, and exactly
// one content_block_stop.
func TestStreamKiroToClaudeSSE_SameToolUseID_MultiEvent(t *testing.T) {
	raw := `{"content": "Let me check."}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "{\"loc\":", "stop": false}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "\"NYC\"}", "stop": true}`
	reader := strings.NewReader(raw)
	var lines []string
	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, string(line))
	})

	// Count tool-related SSE events.
	var toolStarts, inputDeltas, toolStops int
	for _, s := range lines {
		if strings.Contains(s, `"tool_use"`) && strings.Contains(s, "content_block_start") {
			toolStarts++
		}
		if strings.Contains(s, "input_json_delta") {
			inputDeltas++
		}
		// content_block_stop for tool (look for stop after the tool block start)
		if strings.Contains(s, "content_block_stop") {
			toolStops++
		}
	}

	if toolStarts != 1 {
		t.Errorf("expected exactly 1 tool_use content_block_start, got %d", toolStarts)
	}
	if inputDeltas < 2 {
		t.Errorf("expected at least 2 input_json_delta events (one per shard), got %d", inputDeltas)
	}
	// Verify the first shard is emitted on the initial toolUse event, and the
	// second shard on the continuation event.
	var shards []string
	for _, s := range lines {
		if strings.Contains(s, "input_json_delta") {
			// Extract partial_json value.
			idx := strings.Index(s, "partial_json")
			if idx >= 0 {
				shards = append(shards, s[idx:])
			}
		}
	}
	if len(shards) < 2 {
		t.Fatalf("expected at least 2 input_json_delta shards, got %d", len(shards))
	}
	if !strings.Contains(shards[0], `loc`) {
		t.Errorf("first shard should contain initial input fragment, got: %s", shards[0])
	}
	if !strings.Contains(shards[1], `NYC`) {
		t.Errorf("second shard should contain continuation input, got: %s", shards[1])
	}
}

// TestBuildClaudeMessageJSON_MultiTool_DifferentIDs verifies that sequential
// tool calls with different toolUseIDs produce separate tool_use blocks, and
// the first tool is properly finalized before the second starts.
func TestBuildClaudeMessageJSON_MultiTool_DifferentIDs(t *testing.T) {
	raw := `{"content": "Running tools."}` +
		`binary{"name": "read_file", "toolUseId": "tu-a", "input": "{\"path\":\"/a\"}", "stop": true}` +
		`binary{"name": "write_file", "toolUseId": "tu-b", "input": "{\"path\":\"/b\"}", "stop": true}`
	maps := helps.BuildKiroToolNameMaps(nil)
	result := buildClaudeMessageJSON([]byte(raw), maps, "claude-sonnet-4-5")

	var msg map[string]interface{}
	if err := json.Unmarshal(result, &msg); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if msg["stop_reason"] != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %v", msg["stop_reason"])
	}

	content := msg["content"].([]interface{})
	var toolBlocks []map[string]interface{}
	for _, block := range content {
		b := block.(map[string]interface{})
		if b["type"] == "tool_use" {
			toolBlocks = append(toolBlocks, b)
		}
	}
	if len(toolBlocks) != 2 {
		t.Fatalf("expected 2 tool_use blocks, got %d", len(toolBlocks))
	}
	if toolBlocks[0]["id"] != "tu-a" || toolBlocks[0]["name"] != "read_file" {
		t.Errorf("first tool block mismatch: id=%v name=%v", toolBlocks[0]["id"], toolBlocks[0]["name"])
	}
	if toolBlocks[1]["id"] != "tu-b" || toolBlocks[1]["name"] != "write_file" {
		t.Errorf("second tool block mismatch: id=%v name=%v", toolBlocks[1]["id"], toolBlocks[1]["name"])
	}
}

// TestStreamKiroToClaudeSSE_MultiTool_DifferentIDs verifies that streaming
// with two sequential tool calls (different toolUseIDs) emits two separate
// content_block_start events and the first tool's block is stopped before the
// second starts.
func TestStreamKiroToClaudeSSE_MultiTool_DifferentIDs(t *testing.T) {
	raw := `{"content": "Tools."}` +
		`binary{"name": "read_file", "toolUseId": "tu-a", "input": "{\"p\":\"/a\"}", "stop": false}` +
		`binary{"stop": true}` +
		`binary{"name": "write_file", "toolUseId": "tu-b", "input": "{\"p\":\"/b\"}", "stop": true}`
	reader := strings.NewReader(raw)
	var lines []string
	streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, string(line))
	})

	var toolStarts, blockStops int
	var toolNames []string
	for _, s := range lines {
		if strings.Contains(s, `"tool_use"`) && strings.Contains(s, "content_block_start") {
			toolStarts++
			// Extract tool name for ordering check.
			if strings.Contains(s, "read_file") {
				toolNames = append(toolNames, "read_file")
			} else if strings.Contains(s, "write_file") {
				toolNames = append(toolNames, "write_file")
			}
		}
		if strings.Contains(s, "content_block_stop") {
			blockStops++
		}
	}

	if toolStarts != 2 {
		t.Errorf("expected 2 tool_use content_block_start events, got %d", toolStarts)
	}
	if len(toolNames) != 2 || toolNames[0] != "read_file" || toolNames[1] != "write_file" {
		t.Errorf("expected tools in order [read_file, write_file], got %v", toolNames)
	}
	// At least 3 block stops: text block + tool-a + tool-b (possibly more if text is stopped).
	if blockStops < 2 {
		t.Errorf("expected at least 2 content_block_stop events (one per tool), got %d", blockStops)
	}

	// Verify first tool's stop appears before second tool's start.
	firstToolStopIdx := -1
	secondToolStartIdx := -1
	for i, s := range lines {
		if strings.Contains(s, "content_block_stop") && firstToolStopIdx < 0 {
			// Skip text block stop — look for stops after the first tool start.
			for j := 0; j < i; j++ {
				if strings.Contains(lines[j], `"tool_use"`) && strings.Contains(lines[j], "content_block_start") {
					firstToolStopIdx = i
					break
				}
			}
		}
		if strings.Contains(s, "write_file") && strings.Contains(s, "content_block_start") {
			secondToolStartIdx = i
		}
	}
	if firstToolStopIdx >= 0 && secondToolStartIdx >= 0 && firstToolStopIdx > secondToolStartIdx {
		t.Error("first tool's content_block_stop should appear before second tool's content_block_start")
	}
}

// --- Refresh tests ---

// TestKiroRefresh_BuilderID_MissingCredentials verifies that the builder-id
// refresh path returns a clear local error when clientId or clientSecret is missing.
func TestKiroRefresh_BuilderID_MissingCredentials(t *testing.T) {
	e := NewKiroExecutor(nil)

	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{
			name: "missing clientId",
			metadata: map[string]any{
				"refreshToken": "rt-test",
				"authMethod":   "builder-id",
				"clientSecret": "secret-123",
			},
		},
		{
			name: "missing clientSecret",
			metadata: map[string]any{
				"refreshToken": "rt-test",
				"authMethod":   "builder-id",
				"clientId":     "id-123",
			},
		},
		{
			name: "missing both",
			metadata: map[string]any{
				"refreshToken": "rt-test",
				"authMethod":   "builder-id",
			},
		},
		{
			name: "builder-id whitespace-only clientId",
			metadata: map[string]any{
				"refreshToken": "rt-test",
				"authMethod":   "builder-id",
				"clientId":     "  ",
				"clientSecret": "secret-123",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Provider: "kiro",
				Metadata: tt.metadata,
			}
			_, err := e.Refresh(context.Background(), auth)
			if err == nil {
				t.Fatal("expected error for missing builder-id credentials, got nil")
			}
			if !strings.Contains(err.Error(), "clientId and clientSecret") {
				t.Errorf("expected error mentioning clientId and clientSecret, got: %v", err)
			}
		})
	}
}

// TestKiroRefresh_BuilderID_Success verifies the happy path for builder-id refresh.
func TestKiroRefresh_BuilderID_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request body contains required fields.
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("failed to parse request body: %v", err)
		}
		if req["clientId"] != "id-123" {
			t.Errorf("expected clientId=id-123, got %s", req["clientId"])
		}
		if req["clientSecret"] != "secret-456" {
			t.Errorf("expected clientSecret=secret-456, got %s", req["clientSecret"])
		}
		if req["grantType"] != "refresh_token" {
			t.Errorf("expected grantType=refresh_token, got %s", req["grantType"])
		}
		if req["refreshToken"] != "rt-old" {
			t.Errorf("expected refreshToken=rt-old, got %s", req["refreshToken"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "at-new",
			"refreshToken": "rt-new",
			"expiresIn":    3600,
		})
	}))
	defer ts.Close()

	// Patch the IDC refresh URL to point to our test server.
	origTemplate := helps.KiroIDCRefreshURLTemplate
	helps.KiroIDCRefreshURLTemplate = ts.URL + "/token"
	defer func() { helps.KiroIDCRefreshURLTemplate = origTemplate }()

	e := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		Provider: "kiro",
		Metadata: map[string]any{
			"refreshToken": "rt-old",
			"authMethod":   "builder-id",
			"clientId":     "id-123",
			"clientSecret": "secret-456",
		},
	}

	result, err := e.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kiroMetaStr(result, "accessToken") != "at-new" {
		t.Errorf("expected accessToken=at-new, got %s", kiroMetaStr(result, "accessToken"))
	}
	if kiroMetaStr(result, "refreshToken") != "rt-new" {
		t.Errorf("expected refreshToken=rt-new, got %s", kiroMetaStr(result, "refreshToken"))
	}
	if kiroMetaStr(result, "authMethod") != "builder-id" {
		t.Errorf("expected authMethod=builder-id, got %s", kiroMetaStr(result, "authMethod"))
	}
}

// TestKiroRefresh_Social_Success verifies the happy path for social refresh.
func TestKiroRefresh_Social_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("failed to parse request body: %v", err)
		}
		// Social refresh should NOT contain clientId/clientSecret.
		if _, ok := req["clientId"]; ok {
			t.Error("social refresh should not include clientId")
		}
		if _, ok := req["clientSecret"]; ok {
			t.Error("social refresh should not include clientSecret")
		}
		if req["refreshToken"] != "rt-social" {
			t.Errorf("expected refreshToken=rt-social, got %s", req["refreshToken"])
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken":  "at-social-new",
			"refreshToken": "rt-social-new",
			"profileArn":   "arn:aws:kiro:us-east-1:123456:profile/test",
			"expiresIn":    7200,
		})
	}))
	defer ts.Close()

	// Patch the social refresh URL to point to our test server.
	origTemplate := helps.KiroSocialRefreshURLTemplate
	helps.KiroSocialRefreshURLTemplate = ts.URL + "/refreshToken"
	defer func() { helps.KiroSocialRefreshURLTemplate = origTemplate }()

	e := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		Provider: "kiro",
		Metadata: map[string]any{
			"refreshToken": "rt-social",
			"authMethod":   "social",
		},
	}

	result, err := e.Refresh(context.Background(), auth)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if kiroMetaStr(result, "accessToken") != "at-social-new" {
		t.Errorf("expected accessToken=at-social-new, got %s", kiroMetaStr(result, "accessToken"))
	}
	if kiroMetaStr(result, "refreshToken") != "rt-social-new" {
		t.Errorf("expected refreshToken=rt-social-new, got %s", kiroMetaStr(result, "refreshToken"))
	}
	if kiroMetaStr(result, "profileArn") != "arn:aws:kiro:us-east-1:123456:profile/test" {
		t.Errorf("expected profileArn, got %s", kiroMetaStr(result, "profileArn"))
	}
	if kiroMetaStr(result, "authMethod") != "social" {
		t.Errorf("expected authMethod=social, got %s", kiroMetaStr(result, "authMethod"))
	}
}

// TestKiroThinkingAdaptiveEffort verifies that adaptive thinking effort from
// output_config.effort (Claude canonical format) is correctly read by
// buildKiroCodeWhispererRequest after ApplyThinking processes it.
func TestKiroThinkingAdaptiveEffort(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)
	processed, err := thinking.ApplyThinking(body, "claude-sonnet-4-6(high)", "claude", "claude", "kiro")
	if err != nil {
		t.Fatalf("ApplyThinking error: %v", err)
	}

	// Claude 4.6 models support adaptive thinking; ApplyThinking should set type=adaptive + output_config.effort.
	thinkingType := gjson.GetBytes(processed, "thinking.type").String()
	effort := gjson.GetBytes(processed, "output_config.effort").String()
	if thinkingType != "adaptive" {
		t.Errorf("thinking.type = %q, want adaptive", thinkingType)
	}
	if effort != "high" {
		t.Errorf("output_config.effort = %q, want high", effort)
	}

	// Verify buildKiroCodeWhispererRequest reads the effort and generates the correct prefix.
	cwReq, _, errBuild := buildKiroCodeWhispererRequest(processed, nil)
	if errBuild != nil {
		t.Fatalf("buildKiroCodeWhispererRequest error: %v", errBuild)
	}

	cwStr := string(cwReq)
	if !strings.Contains(cwStr, "adaptive") {
		t.Error("expected adaptive thinking_mode in CodeWhisperer request")
	}
	if !strings.Contains(cwStr, "thinking_effort") {
		t.Error("expected thinking_effort tag in CodeWhisperer request")
	}
	if !strings.Contains(cwStr, "high") {
		t.Error("expected effort=high in CodeWhisperer request")
	}
}

// --- HIGH-3 (P0-2) error classification + 401 force-refresh-once retry tests ---

// patchKiroEndpoints rewires the Kiro base URL and social refresh URL templates
// to a single httptest.Server. The Kiro path responds via baseHandler and the
// refresh path responds via refreshHandler. The returned cleanup function
// restores the original templates.
func patchKiroEndpoints(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	origBase := helps.KiroBaseURLTemplate
	origSocial := helps.KiroSocialRefreshURLTemplate
	helps.KiroBaseURLTemplate = server.URL + "/generateAssistantResponse"
	helps.KiroSocialRefreshURLTemplate = server.URL + "/refreshToken"
	return func() {
		helps.KiroBaseURLTemplate = origBase
		helps.KiroSocialRefreshURLTemplate = origSocial
	}
}

// kiroScript routes Kiro test requests by URL path. Each call to the
// generateAssistantResponse handler advances the response index so a sequence
// of upstream replies (e.g. 401 then 200) can be scripted deterministically.
type kiroScript struct {
	mainCalls        int32
	refreshCalls     int32
	mainResponses    []func(http.ResponseWriter, *http.Request)
	refreshResponses []func(http.ResponseWriter, *http.Request)
	t                *testing.T
}

func (s *kiroScript) handler() http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generateAssistantResponse"):
			idx := atomic.AddInt32(&s.mainCalls, 1) - 1
			if int(idx) >= len(s.mainResponses) {
				s.t.Fatalf("Kiro main handler received unexpected call #%d (only %d scripted)", idx+1, len(s.mainResponses))
				return
			}
			s.mainResponses[idx](w, r)
		case strings.HasSuffix(r.URL.Path, "/refreshToken"):
			idx := atomic.AddInt32(&s.refreshCalls, 1) - 1
			if int(idx) >= len(s.refreshResponses) {
				s.t.Fatalf("Kiro refresh handler received unexpected call #%d (only %d scripted)", idx+1, len(s.refreshResponses))
				return
			}
			s.refreshResponses[idx](w, r)
		default:
			s.t.Fatalf("Kiro handler received unexpected path: %s", r.URL.Path)
		}
	})
}

func newKiroAuth(authID, accessToken string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       authID,
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken":  accessToken,
			"refreshToken": "rt-test",
			"region":       "us-east-1",
			"authMethod":   helps.KiroAuthMethodSocial,
		},
	}
}

func successKiroResponse(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
}

func errorKiroResponse(status int, body string, headers map[string]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func successRefresh(body map[string]interface{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

// TestKiroExecute_Status401_RefreshAndRetry_Succeeds verifies that a 401 from
// Kiro triggers exactly one bounded force refresh and one retry; if the retry
// returns 200, Execute returns success and surfaces the new accessToken on the
// auth metadata in-place.
func TestKiroExecute_Status401_RefreshAndRetry_Succeeds(t *testing.T) {
	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusUnauthorized, `{"error":"expired token"}`, nil),
			func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
					t.Fatalf("retry Authorization = %q, want Bearer at-new", got)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(`binary{"content":"after-refresh"}`))
			},
		},
		refreshResponses: []func(http.ResponseWriter, *http.Request){
			successRefresh(map[string]interface{}{
				"accessToken":  "at-new",
				"refreshToken": "rt-rotated",
				"expiresIn":    3600,
			}),
		},
	}
	server := httptest.NewServer(script.handler())
	defer server.Close()
	defer patchKiroEndpoints(t, server)()

	auth := newKiroAuth("kiro-401-success", "at-old")
	executor := NewKiroExecutor(nil)
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute err = %v, want nil", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "after-refresh" {
		t.Fatalf("response content = %q, want after-refresh; payload=%s", got, string(resp.Payload))
	}
	if atomic.LoadInt32(&script.mainCalls) != 2 {
		t.Fatalf("main calls = %d, want 2 (initial + 1 retry)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 force refresh", script.refreshCalls)
	}
	if got := kiroMetaStr(auth, "accessToken"); got != "at-new" {
		t.Fatalf("auth accessToken = %q, want at-new", got)
	}
}

// TestKiroExecute_Status401_RefreshAndRetry_FailsAfterRetry verifies the bound:
// when the retry also returns 401, Execute propagates a classified KiroError
// with status 401 and class "unauthorized", and only one refresh was attempted.
func TestKiroExecute_Status401_RefreshAndRetry_FailsAfterRetry(t *testing.T) {
	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusUnauthorized, `{"error":"expired"}`, nil),
			errorKiroResponse(http.StatusUnauthorized, `{"error":"still expired"}`, nil),
		},
		refreshResponses: []func(http.ResponseWriter, *http.Request){
			successRefresh(map[string]interface{}{
				"accessToken":  "at-new",
				"refreshToken": "rt-rotated",
				"expiresIn":    3600,
			}),
		},
	}
	server := httptest.NewServer(script.handler())
	defer server.Close()
	defer patchKiroEndpoints(t, server)()

	auth := newKiroAuth("kiro-401-fail", "at-old")
	executor := NewKiroExecutor(nil)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err == nil {
		t.Fatalf("Execute err = nil, want classified 401 error")
	}
	var ke *helps.KiroError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *helps.KiroError, got %T: %v", err, err)
	}
	if ke.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", ke.StatusCode())
	}
	if ke.Classification() != helps.KiroErrUnauthorized {
		t.Fatalf("Classification = %q, want %q", ke.Classification(), helps.KiroErrUnauthorized)
	}
	if atomic.LoadInt32(&script.mainCalls) != 2 {
		t.Fatalf("main calls = %d, want exactly 2 (no extra retries beyond the bounded one)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1 bounded force refresh", script.refreshCalls)
	}
}

// TestKiroExecute_Status401_RefreshFails_NoRetry verifies that if the bounded
// refresh itself fails, Execute does NOT issue a second main call and instead
// surfaces the original classified 401.
func TestKiroExecute_Status401_RefreshFails_NoRetry(t *testing.T) {
	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusUnauthorized, `{"error":"expired"}`, nil),
		},
		refreshResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusBadRequest, `{"error":"invalid_grant"}`, nil),
		},
	}
	server := httptest.NewServer(script.handler())
	defer server.Close()
	defer patchKiroEndpoints(t, server)()

	auth := newKiroAuth("kiro-401-refresh-fail", "at-old")
	executor := NewKiroExecutor(nil)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err == nil {
		t.Fatal("Execute err = nil, want classified 401 error after refresh failure")
	}
	var ke *helps.KiroError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *helps.KiroError, got %T: %v", err, err)
	}
	if ke.Classification() != helps.KiroErrUnauthorized {
		t.Fatalf("Classification = %q, want unauthorized", ke.Classification())
	}
	if atomic.LoadInt32(&script.mainCalls) != 1 {
		t.Fatalf("main calls = %d, want exactly 1 (no retry after refresh failure)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", script.refreshCalls)
	}
}

// TestKiroExecute_StatusErrors_NoRefreshNoRetry covers 402, 403, 429, and 5xx:
// each must return a classified KiroError without invoking refresh and without
// retrying the upstream call.
func TestKiroExecute_StatusErrors_NoRefreshNoRetry(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		headers   map[string]string
		wantClass helps.KiroErrorClass
		wantRetry bool // expect non-zero RetryAfter
	}{
		{
			name:      "402 quota exhausted",
			status:    402,
			body:      `{"error":"monthly limit"}`,
			wantClass: helps.KiroErrQuotaExhausted,
		},
		{
			name:      "403 forbidden policy",
			status:    403,
			body:      `{"error":"profile policy denied"}`,
			wantClass: helps.KiroErrForbidden,
		},
		{
			name:      "429 rate limited with Retry-After",
			status:    429,
			body:      `{"error":"too many requests"}`,
			headers:   map[string]string{"Retry-After": "5"},
			wantClass: helps.KiroErrRateLimited,
			wantRetry: true,
		},
		{
			name:      "500 internal server error",
			status:    500,
			body:      `internal error`,
			wantClass: helps.KiroErrServer,
		},
		{
			name:      "503 service unavailable",
			status:    503,
			body:      `service unavailable`,
			wantClass: helps.KiroErrServer,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &kiroScript{
				t: t,
				mainResponses: []func(http.ResponseWriter, *http.Request){
					errorKiroResponse(tt.status, tt.body, tt.headers),
				},
			}
			server := httptest.NewServer(script.handler())
			defer server.Close()
			defer patchKiroEndpoints(t, server)()

			auth := newKiroAuth("kiro-"+string(tt.wantClass), "at-test")
			executor := NewKiroExecutor(nil)
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-5",
				Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
			if err == nil {
				t.Fatalf("Execute err = nil, want classified error for status %d", tt.status)
			}
			var ke *helps.KiroError
			if !errors.As(err, &ke) {
				t.Fatalf("expected *helps.KiroError, got %T: %v", err, err)
			}
			if ke.StatusCode() != tt.status {
				t.Fatalf("StatusCode = %d, want %d", ke.StatusCode(), tt.status)
			}
			if ke.Classification() != tt.wantClass {
				t.Fatalf("Classification = %q, want %q", ke.Classification(), tt.wantClass)
			}
			if tt.wantRetry {
				if ra := ke.RetryAfter(); ra == nil || *ra <= 0 {
					t.Fatalf("RetryAfter = %v, want positive duration", ra)
				}
			}
			if atomic.LoadInt32(&script.mainCalls) != 1 {
				t.Fatalf("main calls = %d, want exactly 1 (no retry)", script.mainCalls)
			}
			if atomic.LoadInt32(&script.refreshCalls) != 0 {
				t.Fatalf("refresh calls = %d, want exactly 0 (no refresh)", script.refreshCalls)
			}
		})
	}
}

// TestKiroExecute_NetworkError_Classified verifies that a transport-level
// failure (e.g. server hijacks then closes the connection without a status)
// is wrapped as a network-classified KiroError without invoking refresh.
func TestKiroExecute_NetworkError_Classified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack error: %v", err)
		}
		_ = conn.Close()
	}))
	defer server.Close()
	origBase := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL + "/generateAssistantResponse"
	defer func() { helps.KiroBaseURLTemplate = origBase }()

	auth := newKiroAuth("kiro-net", "at-test")
	executor := NewKiroExecutor(nil)
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err == nil {
		t.Fatal("Execute err = nil, want network-classified error")
	}
	var ke *helps.KiroError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *helps.KiroError, got %T: %v", err, err)
	}
	if ke.Classification() != helps.KiroErrNetwork {
		t.Fatalf("Classification = %q, want %q", ke.Classification(), helps.KiroErrNetwork)
	}
	if ke.StatusCode() != 0 {
		t.Fatalf("StatusCode = %d, want 0 for network error", ke.StatusCode())
	}
}

// TestKiroExecuteStream_Status401_RefreshAndRetry verifies the streaming path
// also benefits from the bounded force-refresh-once retry.
func TestKiroExecuteStream_Status401_RefreshAndRetry(t *testing.T) {
	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusUnauthorized, `{"error":"expired"}`, nil),
			func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer at-stream-new" {
					t.Fatalf("retry Authorization = %q, want Bearer at-stream-new", got)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(`binary{"content":"stream-after-refresh"}`))
			},
		},
		refreshResponses: []func(http.ResponseWriter, *http.Request){
			successRefresh(map[string]interface{}{
				"accessToken":  "at-stream-new",
				"refreshToken": "rt-stream-rotated",
				"expiresIn":    3600,
			}),
		},
	}
	server := httptest.NewServer(script.handler())
	defer server.Close()
	defer patchKiroEndpoints(t, server)()

	auth := newKiroAuth("kiro-stream-401", "at-stream-old")
	executor := NewKiroExecutor(nil)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream err = %v, want nil after refresh retry", err)
	}
	var sawContent bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "stream-after-refresh") {
			sawContent = true
		}
	}
	if !sawContent {
		t.Fatal("expected stream content to include stream-after-refresh after refresh retry")
	}
	if atomic.LoadInt32(&script.mainCalls) != 2 {
		t.Fatalf("main calls = %d, want 2 (initial + 1 retry)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", script.refreshCalls)
	}
}

// TestKiroExecuteStream_Status402_QuotaExhausted_NoRetry verifies streaming 402
// is classified and not retried.
func TestKiroExecuteStream_Status402_QuotaExhausted_NoRetry(t *testing.T) {
	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusPaymentRequired, `{"error":"quota"}`, nil),
		},
	}
	server := httptest.NewServer(script.handler())
	defer server.Close()
	defer patchKiroEndpoints(t, server)()

	auth := newKiroAuth("kiro-stream-402", "at-test")
	executor := NewKiroExecutor(nil)
	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err == nil {
		t.Fatal("ExecuteStream err = nil, want classified 402")
	}
	var ke *helps.KiroError
	if !errors.As(err, &ke) {
		t.Fatalf("expected *helps.KiroError, got %T: %v", err, err)
	}
	if ke.Classification() != helps.KiroErrQuotaExhausted {
		t.Fatalf("Classification = %q, want quota_exhausted", ke.Classification())
	}
	if atomic.LoadInt32(&script.mainCalls) != 1 {
		t.Fatalf("main calls = %d, want 1 (no retry)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 0 {
		t.Fatalf("refresh calls = %d, want 0 (no refresh on 402)", script.refreshCalls)
	}
}
