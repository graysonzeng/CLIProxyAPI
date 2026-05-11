package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"

	// Register Claude thinking provider applier (needed by ApplyThinking tests).
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
)

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
