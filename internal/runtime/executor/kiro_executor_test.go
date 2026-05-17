package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

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
	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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

	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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
	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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

// TestStreamKiroToClaudeSSE_ToolUseContentBlockStartIncludesEmptyInput pins the
// Anthropic Messages API SSE contract: every `content_block_start` event for a
// `tool_use` block must publish an initial `input` value (an empty object) in
// its content_block payload. Strict downstream SDK parsers — notably Claude
// Code v2.x — validate this synchronously as the event arrives. When the field
// is missing, Claude Code reports the tool call as "Invalid tool parameters"
// or remains stuck on "Initializing…" because input_json_delta accumulators
// have nothing to merge into. This test mirrors AIClient2API claude-kiro.js
// (`input: {}`) and kiro.rs anthropic/stream.rs (`"input": {}`).
//
// We assert the exact wire shape (including the empty-object `input` field)
// across three representative streaming patterns Kiro uses in production:
//  1. start-with-input then stop in a separate event
//  2. start without input then input chunks then stop
//  3. start-with-full-input-and-stop in a single event
//
// Because the proxy emits standard Claude SSE for downstream Claude clients
// regardless of how Kiro framed the upstream events, all three must produce a
// content_block_start whose content_block has the canonical
// {"type":"tool_use","id":...,"name":...,"input":{}} shape.
func TestStreamKiroToClaudeSSE_ToolUseContentBlockStartIncludesEmptyInput(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantToolID  string
		wantName    string
		wantHasName bool
	}{
		{
			name:       "single event start with input shard",
			raw:        `binary{"name": "get_weather", "toolUseId": "tu-a", "input": "{\"loc\":\"NYC\"}", "stop": false}binary{"stop": true}`,
			wantToolID: "tu-a",
			wantName:   "get_weather",
		},
		{
			name:       "split start then input chunks then stop",
			raw:        `binary{"name": "bash", "toolUseId": "tu-b", "stop": false}binary{"input": "{\"cmd\":\"ls\"}"}binary{"stop": true}`,
			wantToolID: "tu-b",
			wantName:   "bash",
		},
		{
			name:       "single event start with full input and stop",
			raw:        `binary{"name": "read_file", "toolUseId": "tu-c", "input": "{\"path\":\"/etc/hosts\"}", "stop": true}`,
			wantToolID: "tu-c",
			wantName:   "read_file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := strings.NewReader(tc.raw)
			var lines [][]byte
			_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-opus-4-6", func(line []byte) {
				lines = append(lines, append([]byte(nil), line...))
			})

			var startData []byte
			for _, line := range lines {
				dataLines := claudeSSEDataLines(line)
				for _, dl := range dataLines {
					payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
					evtType := gjson.GetBytes(payload, "type").String()
					blockType := gjson.GetBytes(payload, "content_block.type").String()
					if evtType == "content_block_start" && blockType == "tool_use" {
						startData = bytes.Clone(payload)
					}
				}
			}

			if len(startData) == 0 {
				t.Fatalf("expected a tool_use content_block_start event, got SSE lines: %s", strings.Join(stringifyLines(lines), "|"))
			}

			block := gjson.GetBytes(startData, "content_block")
			if block.Get("id").String() != tc.wantToolID {
				t.Errorf("content_block.id = %q, want %q", block.Get("id").String(), tc.wantToolID)
			}
			if block.Get("name").String() != tc.wantName {
				t.Errorf("content_block.name = %q, want %q", block.Get("name").String(), tc.wantName)
			}
			input := block.Get("input")
			if !input.Exists() {
				t.Fatalf("content_block.input field is missing in content_block_start event: %s", string(startData))
			}
			if !input.IsObject() {
				t.Fatalf("content_block.input must be an object, got type %v: %s", input.Type, string(startData))
			}
			// Initial `input` must be the empty object so that downstream
			// SDKs can apply input_json_delta partial_json shards over a
			// well-formed initial value, and so that an empty-input tool
			// call still produces a parseable {} rather than an
			// undefined/null parameter set.
			var inputMap map[string]interface{}
			if err := json.Unmarshal([]byte(input.Raw), &inputMap); err != nil {
				t.Fatalf("content_block.input must parse as JSON object: %v (raw=%s)", err, input.Raw)
			}
			if len(inputMap) != 0 {
				t.Errorf("content_block.input must be the empty object on block start, got %v", inputMap)
			}
		})
	}
}

// TestStreamKiroToClaudeSSE_DropsEmptyToolUseBeforeNextTool covers the
// Claude Code "Invalid tool parameters" regression seen with Kiro Opus 4.6:
// Kiro can emit a toolUse start event with no input, then immediately move on
// to a different tool. If the proxy forwards that empty start as a Claude
// tool_use block (`input: {}`), Claude Code executes it and reports missing
// required parameters such as Bash.command, Read.file_path, Agent.prompt, etc.
//
// The proxy must therefore delay publishing a Kiro tool_use until at least one
// input shard arrives. Empty abandoned tool starts are dropped instead of
// becoming invalid downstream tool calls.
func TestStreamKiroToClaudeSSE_DropsEmptyToolUseBeforeNextTool(t *testing.T) {
	raw := `binary{"name":"Bash","toolUseId":"empty-bash","stop":false}` +
		`binary{"name":"Read","toolUseId":"valid-read","input":"{\"file_path\":\"/tmp/a\"}","stop":true}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, _ = streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})

	var toolStarts []gjson.Result
	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "content_block_start" &&
				gjson.GetBytes(payload, "content_block.type").String() == "tool_use" {
				toolStarts = append(toolStarts, gjson.ParseBytes(payload))
			}
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}

	if len(toolStarts) != 1 {
		t.Fatalf("expected only the valid tool_use to be emitted, got %d starts: %s", len(toolStarts), strings.Join(stringifyLines(lines), "|"))
	}
	block := toolStarts[0].Get("content_block")
	if block.Get("id").String() != "valid-read" {
		t.Fatalf("emitted wrong tool id: got %q, want valid-read", block.Get("id").String())
	}
	if block.Get("name").String() != "Read" {
		t.Fatalf("emitted wrong tool name: got %q, want Read", block.Get("name").String())
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use because a valid tool was emitted", stopReason)
	}
}

func TestStreamKiroToClaudeSSE_DropsIncompleteToolInput(t *testing.T) {
	raw := `binary{"name":"Agent","toolUseId":"bad-agent","stop":false}` +
		`binary{"input":"{\"description\":\"Explore\",\"prompt\":\"unterminated"}` +
		`binary{"stop":true}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, _ = streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})

	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "content_block.type").String() == "tool_use" {
				t.Fatalf("incomplete tool input must not be emitted: %s", strings.Join(stringifyLines(lines), "|"))
			}
		}
	}
}

func TestStreamKiroToClaudeSSE_VisiblePreambleWithIncompleteToolCanEndTurn(t *testing.T) {
	raw := `binary{"content":"I will explore the workspace."}` +
		`binary{"name":"Agent","toolUseId":"bad-agent","stop":false}` +
		`binary{"input":"{\"description\":\"Explore\",\"prompt\":\"unterminated"}` +
		`binary{"stop":true}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}
	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn for complete visible preamble with dropped tool; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ErrsOnThinkingOnlyStream(t *testing.T) {
	raw := `binary{"content":"<thinking>\nNeed to inspect the workspace before answering."}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatalf("expected thinking-only stream to return an error, got nil; lines=%s", strings.Join(stringifyLines(lines), "|"))
	}
	if !strings.Contains(err.Error(), "without visible content or tool use") {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.payloadStarted || len(lines) != 0 {
		t.Fatalf("thinking-only stream must fail before emitting payload; result=%+v lines=%s", result, strings.Join(stringifyLines(lines), "|"))
	}
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				t.Fatalf("thinking-only stream must not be finalized as end_turn: %s", strings.Join(stringifyLines(lines), "|"))
			}
		}
	}
}

func TestStreamKiroToClaudeSSE_ContentLengthExceededMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"partial answer"}binary-headers:exception-type` + "\x00" + `ContentLengthExceededException payload`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}
	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_SuspiciousShortIncompleteEndMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"<thinking>\nNeed to summarize carefully.</thinking>\n\n"}` +
		`binary{"content":"基于探索结果，以"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for suspicious incomplete end; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_SuspiciousVisiblePrefixWithoutThinkingMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"探索完成，"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for suspicious visible prefix; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_IntentionOnlyAnswerMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"我来对当前工作区及各子项目做一轮探索。"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for intention-only answer; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeIntentionOnlyAnswerReturnsMalformedBeforePayload(t *testing.T) {
	raw := `binary{"content":"我来对当前工作区及各子项目做一轮探索。"}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want malformed error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamMalformed {
		t.Fatalf("error = %T %v, want KiroErrStreamMalformed", err, err)
	}
	if result.payloadStarted || len(lines) != 0 {
		t.Fatalf("result=%+v lines=%d, want no payload before malformed error", result, len(lines))
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeIntentionPreambleWithInvalidToolUseReturnsMalformedBeforePayload(t *testing.T) {
	raw := `binary{"content":"我来探索工作区的各个子项目模块。"}` +
		`binary{"name":"Agent","toolUseId":"bad-agent","input":"{\"prompt\":\"read project","stop":false}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want malformed error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamMalformed {
		t.Fatalf("error = %T %v, want KiroErrStreamMalformed", err, err)
	}
	if result.payloadStarted || len(lines) != 0 {
		t.Fatalf("result=%+v lines=%d, want no payload before malformed error", result, len(lines))
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeSplitIntentionPreambleWithInvalidToolUseReturnsMalformedBeforePayload(t *testing.T) {
	raw := `binary{"content":"我"}` +
		`binary{"content":"来探索工作区的各个子项目模块。"}` +
		`binary{"name":"Agent","toolUseId":"bad-agent","input":"{\"prompt\":\"read project","stop":false}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want malformed error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamMalformed {
		t.Fatalf("error = %T %v, want KiroErrStreamMalformed", err, err)
	}
	if result.payloadStarted || len(lines) != 0 {
		t.Fatalf("result=%+v lines=%d, want no payload before malformed error", result, len(lines))
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeCompletionPreambleMalformedReturnsMalformedBeforePayload(t *testing.T) {
	raw := `binary{"content":"探索完成，"}binary{"content":"oops"`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want malformed error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamMalformed {
		t.Fatalf("error = %T %v, want KiroErrStreamMalformed", err, err)
	}
	if result.payloadStarted || len(lines) != 0 {
		t.Fatalf("result=%+v lines=%d, want no payload before malformed error", result, len(lines))
	}
}

func TestStreamKiroToClaudeSSE_LetMeValidatePreambleMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"根据 CLAUDE.md 中的项目地图，这是一个 StarRocks AIOps 统一工作区，包含 14 个协作项目。让我快速验证各模块的实际状态。"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for validation preamble; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_MarkdownHeadingFragmentMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"\n##"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for markdown heading fragment; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ShortOpusContinuationCommaMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"口，"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for short Opus comma continuation; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ContinuationAfterIncompleteAssistantSingleRuneMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"据"}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroContinuationAfterIncompleteAssistantKey{}, true)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for single-rune continuation; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ContinuationAfterIncompleteAssistantFragmentMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"引擎解析执行计划 "}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroContinuationAfterIncompleteAssistantKey{}, true)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for incomplete continuation fragment; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeContinuationDoesNotMapToMaxTokens(t *testing.T) {
	raw := `binary{"content":"引擎解析执行计划 "}`
	reader := strings.NewReader(raw)
	ctx := context.WithValue(context.Background(), kiroContinuationAfterIncompleteAssistantKey{}, true)
	ctx = context.WithValue(ctx, kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn for Claude Code continuation fragment; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_SynthesizesClosureForLongTruncatedSummary(t *testing.T) {
	ctx := context.WithValue(context.Background(), kiroContinuationAfterIncompleteAssistantKey{}, true)
	raw := `binary{"content":"- **starrocks-profile**：Profile解析\n- **starrocks-profile-mcp**：MCP工具\n- **starrocks-ops-mcp**：运维工具\n- **starrocks-profile-agent**：诊断Agent\n- **starrocks-aiops**：AIOps引擎\n- **starrocks-board**：Vue运维看板，逐步被aiops替"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(ctx, reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	joined := strings.Join(stringifyLines(lines), "|")
	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn for synthesized closure; lines=%s", stopReason, joined)
	}
	if !strings.Contains(joined, "以上为工作区模块功能概览。") {
		t.Fatalf("synthesized closure missing; lines=%s", joined)
	}
}

func TestStreamKiroToClaudeSSE_UnclosedVisibleDelimiterMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"| **starrocks-experience-docs** | 运维实战笔记（查"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for unclosed visible delimiter; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_UnclosedVisibleCodeFenceMapsToMaxTokens(t *testing.T) {
	raw := "binary{\"content\":\"数据流：\\n```\\n┌─ sta\"}"
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for unclosed visible code fence; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_LongVisibleTextWithoutTerminalMapsToMaxTokens(t *testing.T) {
	raw := `binary{"content":"探索完成，以下是各子项目的功能总结：\n\n**starrocks-profile-mcp** (Python/MCP)\nProfile 分析 MCP Server，暴露 14 个工"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "max_tokens" {
		t.Fatalf("stop_reason = %q, want max_tokens for long visible text without terminal punctuation; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_CompleteTableRowCanEndTurn(t *testing.T) {
	raw := `binary{"content":"| **starrocks-experience-docs** | 运维实战笔记 |"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn for complete table row; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_LightModelLongVisibleTextCanEndTurn(t *testing.T) {
	raw := `binary{"content":"探索完成，以下是各子项目的功能总结：\n\n**starrocks-profile-mcp** (Python/MCP)\nProfile 分析 MCP Server，暴露 14 个工"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-haiku-4-5-20251001", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}

	var stopReason string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn for light-model long visible text; lines=%s", stopReason, strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_ThinkingTagsSplitAcrossEvents(t *testing.T) {
	raw := `binary{"content":"<thinking"}` +
		`binary{"content":">\nprivate reasoning</thinking"}` +
		`binary{"content":">\n\nVisible answer."}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v", err)
	}
	var visible, thinking string
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if delta := gjson.GetBytes(payload, "delta.text"); delta.Exists() {
				visible += delta.String()
			}
			if delta := gjson.GetBytes(payload, "delta.thinking"); delta.Exists() {
				thinking += delta.String()
			}
		}
	}
	if visible != "Visible answer." {
		t.Fatalf("visible text = %q, want %q; lines=%s", visible, "Visible answer.", strings.Join(stringifyLines(lines), "|"))
	}
	if thinking != "private reasoning" {
		t.Fatalf("thinking = %q, want private reasoning; lines=%s", thinking, strings.Join(stringifyLines(lines), "|"))
	}
	if strings.Contains(strings.Join(stringifyLines(lines), "|"), "<thinking") ||
		strings.Contains(strings.Join(stringifyLines(lines), "|"), "</thinking") {
		t.Fatalf("thinking tags leaked into SSE payload: %s", strings.Join(stringifyLines(lines), "|"))
	}
}

func TestStreamKiroToClaudeSSE_InterleavedToolUseIDs(t *testing.T) {
	raw := `binary{"name":"Bash","toolUseId":"tu-a","input":"{\"command\":\"echo", "stop":false}` +
		`binary{"name":"Read","toolUseId":"tu-b","input":"{\"file_path\":\"/tmp/a\"}", "stop":true}` +
		`binary{"input":" ok\"}", "toolUseId":"tu-a"}` +
		`binary{"stop":true, "toolUseId":"tu-a"}`
	reader := strings.NewReader(raw)
	var lines [][]byte
	_, _ = streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})

	type toolBlock struct {
		name  string
		input string
	}
	idxToID := map[int]string{}
	blocks := map[string]toolBlock{}
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			switch gjson.GetBytes(payload, "type").String() {
			case "content_block_start":
				if gjson.GetBytes(payload, "content_block.type").String() == "tool_use" {
					idx := int(gjson.GetBytes(payload, "index").Int())
					id := gjson.GetBytes(payload, "content_block.id").String()
					idxToID[idx] = id
					blocks[id] = toolBlock{name: gjson.GetBytes(payload, "content_block.name").String()}
				}
			case "content_block_delta":
				if gjson.GetBytes(payload, "delta.type").String() == "input_json_delta" {
					idx := int(gjson.GetBytes(payload, "index").Int())
					id := idxToID[idx]
					block := blocks[id]
					block.input += gjson.GetBytes(payload, "delta.partial_json").String()
					blocks[id] = block
				}
			}
		}
	}

	if len(blocks) != 2 {
		t.Fatalf("expected 2 valid tool blocks, got %d: %s", len(blocks), strings.Join(stringifyLines(lines), "|"))
	}
	if got := blocks["tu-a"]; got.name != "Bash" || got.input != `{"command":"echo ok"}` {
		t.Fatalf("tu-a mismatch: %+v", got)
	}
	if got := blocks["tu-b"]; got.name != "Read" || got.input != `{"file_path":"/tmp/a"}` {
		t.Fatalf("tu-b mismatch: %+v", got)
	}
}

func TestKiroShouldGateLightModel(t *testing.T) {
	if !kiroShouldGateLightModel("claude-haiku-4-5") {
		t.Fatal("expected explicit Haiku alias to be gated")
	}
	if !kiroShouldGateLightModel("claude-haiku-4-5-20251001") {
		t.Fatal("dated Claude Code auto-route alias maps to Sonnet and must be gated")
	}
	if !kiroShouldGateLightModel("claude-sonnet-4-6") {
		t.Fatal("Sonnet requests must be gated")
	}
	if kiroShouldGateLightModel("claude-opus-4-6") {
		t.Fatal("Opus main requests must not be gated")
	}
}

func TestKiroModelGateLimit(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		{"claude-haiku-4-5", kiroHaikuMaxConcurrentPerAuth},
		{"claude-haiku-4-5-20251001", kiroSonnetMaxConcurrentPerAuth},
		{"claude-sonnet-4-6", kiroSonnetMaxConcurrentPerAuth},
		{"claude-sonnet-4-5", kiroSonnetMaxConcurrentPerAuth},
		{"claude-opus-4-6", 0},
	}
	for _, tt := range tests {
		got := kiroModelGateLimit(helps.MapKiroModel(tt.model))
		if got != tt.want {
			t.Fatalf("kiroModelGateLimit(MapKiroModel(%q)) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

func TestKiroAutoRoutedDatedHaikuUsesSonnetUpstream(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"hi"}]}`)
	cwReq, _, err := buildKiroCodeWhispererRequest(body, nil, "")
	if err != nil {
		t.Fatalf("buildKiroCodeWhispererRequest error: %v", err)
	}
	got := gjson.GetBytes(cwReq, "conversationState.currentMessage.userInputMessage.modelId").String()
	if got != "claude-sonnet-4.6" {
		t.Fatalf("dated Haiku auto-route modelId = %q, want claude-sonnet-4.6", got)
	}
}

func stringifyLines(lines [][]byte) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, string(l))
	}
	return out
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

type errorAfterChunksReader struct {
	chunks []string
	idx    int
	err    error
}

func (r *errorAfterChunksReader) Read(p []byte) (int, error) {
	if r.idx >= len(r.chunks) {
		return 0, r.err
	}
	n := copy(p, r.chunks[r.idx])
	r.idx++
	return n, nil
}

func TestStreamKiroToClaudeSSE_EmptyStreamLeavesConductorEmptyStreamPath(t *testing.T) {
	result, err := streamKiroToClaudeSSE(context.Background(), strings.NewReader(""), nil, "claude-sonnet-4-5", func([]byte) {
		t.Fatal("empty stream should not emit payload")
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v, want nil for conductor empty_stream path", err)
	}
	if result.eventCount != 0 || result.payloadStarted {
		t.Fatalf("result = %+v, want zero events and no payload", result)
	}
}

func TestStreamKiroToClaudeSSE_ReadErrorBeforePayloadReturnsError(t *testing.T) {
	readErr := errors.New("upstream reset")
	result, err := streamKiroToClaudeSSE(context.Background(), &errorAfterChunksReader{err: readErr}, nil, "claude-sonnet-4-5", func([]byte) {
		t.Fatal("read error before payload should not emit payload")
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want read error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamRead {
		t.Fatalf("error = %T %v, want KiroErrStreamRead", err, err)
	}
	if result.payloadStarted {
		t.Fatalf("payloadStarted = true, want false")
	}
}

func TestStreamKiroToClaudeSSE_MalformedBeforePayloadReturnsError(t *testing.T) {
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(context.Background(), strings.NewReader(`binary{"content":"oops"`), nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want malformed error")
	}
	var kiroErr *helps.KiroError
	if !errors.As(err, &kiroErr) || kiroErr.Classification() != helps.KiroErrStreamMalformed {
		t.Fatalf("error = %T %v, want KiroErrStreamMalformed", err, err)
	}
	if len(lines) != 0 || result.payloadStarted {
		t.Fatalf("lines=%d result=%+v, want no payload before malformed error", len(lines), result)
	}
}

func TestStreamKiroToClaudeSSE_MalformedAfterPayloadStillCompletes(t *testing.T) {
	var lines []string
	result, err := streamKiroToClaudeSSE(context.Background(), strings.NewReader(`binary{"content":"Hello"}binary{"content":"oops"`), nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, string(line))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v, want nil after payload started", err)
	}
	if !result.payloadStarted {
		t.Fatalf("payloadStarted = false, want true")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "text_delta") || !strings.Contains(joined, "Hello") {
		t.Fatalf("stream did not emit first payload: %s", joined)
	}
	if !strings.Contains(joined, "message_stop") {
		t.Fatalf("stream should emit terminal success events after trailing malformed residue: %s", joined)
	}
}

func TestStreamKiroToClaudeSSE_ClaudeCodeMalformedAfterPayloadSynthesizesClosure(t *testing.T) {
	ctx := context.WithValue(context.Background(), kiroClaudeCodeRequestKey{}, true)
	var lines [][]byte
	result, err := streamKiroToClaudeSSE(ctx, strings.NewReader(`binary{"content":"项目：starrocks-profile、starrocks-board、starrocks-cluster、starrocks-profile-mcp、starrocks-ops-mcp、starrocks-aiops、starrocks-gc-detector。以上"}binary{"content":"oops"`), nil, "claude-opus-4-6", func(line []byte) {
		lines = append(lines, append([]byte(nil), line...))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v, want nil after payload started", err)
	}
	if !result.payloadStarted {
		t.Fatalf("payloadStarted = false, want true")
	}
	var stopReason string
	joined := strings.Join(stringifyLines(lines), "|")
	for _, line := range lines {
		for _, dl := range claudeSSEDataLines(line) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(dl, []byte("data:")))
			if gjson.GetBytes(payload, "type").String() == "message_delta" {
				stopReason = gjson.GetBytes(payload, "delta.stop_reason").String()
			}
		}
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn after synthesized closure; lines=%s", stopReason, joined)
	}
	if !strings.Contains(joined, "以上") || !strings.Contains(joined, "为工作区模块功能概览") {
		t.Fatalf("synthesized closure missing from stream: %s", joined)
	}
}

func TestStreamKiroToClaudeSSE_ReadErrorAfterPayloadDoesNotEmitStop(t *testing.T) {
	readErr := errors.New("connection reset")
	reader := &errorAfterChunksReader{chunks: []string{`binary{"content": "Hello"}`}, err: readErr}
	var lines []string
	result, err := streamKiroToClaudeSSE(context.Background(), reader, nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, string(line))
	})
	if err == nil {
		t.Fatal("streamKiroToClaudeSSE error = nil, want read error")
	}
	if !result.payloadStarted {
		t.Fatalf("payloadStarted = false, want true")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "message_start") || !strings.Contains(joined, "text_delta") {
		t.Fatalf("lines missing start/content events: %s", joined)
	}
	if strings.Contains(joined, "message_stop") || strings.Contains(joined, "message_delta") {
		t.Fatalf("stream emitted success terminal events after read error: %s", joined)
	}
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

	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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
	// message_start is emitted after the first valid event, so at minimum 1 line
	// should have been emitted before all chunks are consumed.
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

func TestKiroExecutorExecuteStreamPublishesFailureOnMalformedStream(t *testing.T) {
	capture := registerKiroUsageCapture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-stream-fail" {
			t.Fatalf("Authorization = %q, want Bearer at-stream-fail", got)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"oops"`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	authID := "kiro-usage-stream-fail"
	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       authID,
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-fail",
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

	var gotErr error
	var sawPayload bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
		}
		if len(chunk.Payload) > 0 {
			sawPayload = true
		}
	}
	if gotErr == nil {
		t.Fatal("expected stream chunk error")
	}
	if sawPayload {
		t.Fatal("malformed stream before first event should not emit payload")
	}

	record := waitForKiroUsageRecord(t, capture, authID)
	if !record.Failed {
		t.Fatalf("usage record Failed = false, want true: %+v", record)
	}
}

func TestKiroExecutorExecuteStreamRetriesThinkingOnlyBeforePayload(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`binary{"content":"<thinking>\nNeed one more attempt before answering.</thinking>"}` +
				`binary{"content":"\n"}`))
		case 2:
			_, _ = w.Write([]byte(`binary{"content":"retry ok"}`))
		default:
			t.Fatalf("unexpected extra upstream call")
		}
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-stream-retry-empty",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-retry",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error after retry: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if !strings.Contains(got.String(), "retry ok") {
		t.Fatalf("retry response was not streamed to client: %s", got.String())
	}
}

func TestKiroExecutorExecuteStreamRetriesEmptyStreamBeforePayload(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		switch calls.Add(1) {
		case 1, 2:
			return
		case 3:
			_, _ = w.Write([]byte(`binary{"content":"empty stream retry ok"}`))
		default:
			t.Fatalf("unexpected extra upstream call")
		}
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-stream-retry-empty-before-payload",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-retry-empty-before-payload",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error after retry: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls.Load())
	}
	if !strings.Contains(got.String(), "empty stream retry ok") {
		t.Fatalf("retry response was not streamed to client: %s", got.String())
	}
}

func TestKiroExecutorExecuteStreamSynthesizesClaudeCodeTitleGeneration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Fatalf("title generation should not call Kiro upstream")
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-title-generation",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-title-generation",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":[{"type":"text","text":"<session>\nExplore workspace modules\n</session>"}]}],"system":[{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session. Return JSON with a single \"title\" field."}],"tools":[],"max_tokens":64000,"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)
	headers := http.Header{}
	headers.Set("X-Anthropic-Billing-Header", "cc_version=2.1.143.657; cc_entrypoint=sdk-cli")
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers:      headers,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
	out := got.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("synthetic title stream is missing Claude SSE events: %s", out)
	}
	if !strings.Contains(out, `{\"title\":\"Explore workspace modules\"}`) {
		t.Fatalf("synthetic title stream is missing title JSON: %s", out)
	}
}

func TestKiroExecutorExecuteStreamSynthesizesClaudeCodeTitleGenerationFromSystemBilling(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Fatalf("title generation should not call Kiro upstream")
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-title-generation-system-billing",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-title-generation-system-billing",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":[{"type":"text","text":"<session>\nExplore workspace modules\n</session>"}]}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.143.657; cc_entrypoint=sdk-cli; cch=03299;"},{"type":"text","text":"Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session. Return JSON with a single \"title\" field."}],"tools":[],"max_tokens":64000,"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}}},"stream":true}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestLooksLikeClaudeCodeRequestDetectsToolSet(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash"},{"name":"Read"}]}`)
	if !looksLikeClaudeCodeRequest(body, nil) {
		t.Fatal("looksLikeClaudeCodeRequest = false, want true for Claude Code tool set")
	}
}

func TestKiroExecutorExecuteStreamRetrySuppressesThinkingAfterMalformedBeforePayload(t *testing.T) {
	var calls atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		bodiesMu.Lock()
		bodies = append(bodies, string(raw))
		bodiesMu.Unlock()

		w.Header().Set("Content-Type", "application/octet-stream")
		switch calls.Add(1) {
		case 1:
			_, _ = w.Write([]byte(`binary{"content":"oops"`))
		case 2:
			_, _ = w.Write([]byte(`binary{"content":"retry without thinking ok"}`))
		default:
			t.Fatalf("unexpected extra upstream call")
		}
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-stream-retry-no-thinking",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-retry-no-thinking",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error after retry: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if !strings.Contains(got.String(), "retry without thinking ok") {
		t.Fatalf("retry response was not streamed to client: %s", got.String())
	}

	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("captured request bodies = %d, want 2", len(bodies))
	}
	if strings.Contains(bodies[0], "thinking_mode") || strings.Contains(bodies[0], "thinking_effort") || strings.Contains(bodies[0], "max_thinking_length") {
		t.Fatalf("first Opus streaming request must suppress thinking to avoid hidden-thinking stalls; body=%s", bodies[0])
	}
	if strings.Contains(bodies[1], "thinking_mode") || strings.Contains(bodies[1], "thinking_effort") || strings.Contains(bodies[1], "max_thinking_length") {
		t.Fatalf("retry request must suppress thinking to avoid another hidden-thinking stall; body=%s", bodies[1])
	}
}

func TestKiroExecutorExecuteStreamSuppressesThinkingForClaudeCodeTools(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		captured = string(raw)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"tool-ready answer"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-claude-code-no-thinking",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-claude-code-no-thinking",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{
		"model":"claude-opus-4-6",
		"messages":[{"role":"user","content":"explore"}],
		"thinking":{"type":"enabled","budget_tokens":31999},
		"tools":[
			{"name":"Agent","description":"Run a subagent","input_schema":{"type":"object","properties":{"prompt":{"type":"string"}}}},
			{"name":"Bash","description":"Run shell","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}
		]
	}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}
	if strings.Contains(captured, "thinking_mode") || strings.Contains(captured, "thinking_effort") || strings.Contains(captured, "max_thinking_length") {
		t.Fatalf("Claude Code tool request must suppress thinking to avoid hidden-thinking stalls; body=%s", captured)
	}
	if !strings.Contains(captured, "after using tools always provide the completed summary") {
		t.Fatalf("Claude Code guidance was not attached to Kiro request; body=%s", captured)
	}
}

func TestKiroExecutorExecuteStreamSuppressesThinkingForClaudeCodeHeader(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		captured = string(raw)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"header-detected answer"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-claude-code-header",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-claude-code-header",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"explore"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"X-Anthropic-Billing-Header": []string{"cc_version=2.1.119; cc_entrypoint=sdk-cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}
	if strings.Contains(captured, "thinking_mode") || strings.Contains(captured, "thinking_effort") || strings.Contains(captured, "max_thinking_length") {
		t.Fatalf("Claude Code header request must suppress thinking; body=%s", captured)
	}
	if !strings.Contains(captured, "after using tools always provide the completed summary") {
		t.Fatalf("Claude Code header guidance was not attached; body=%s", captured)
	}
}

func TestKiroExecutorExecuteStreamAddsContinuationGuidanceForIncompleteTail(t *testing.T) {
	var captured string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		captured = string(raw)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(`binary{"content":"finish."}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-continuation-guidance",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-continuation-guidance",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"assistant","content":"- **starrocks-board**：Vue"},{"role":"user","content":[{"type":"text","text":"continue"}]}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}
	if !strings.Contains(captured, "Continuation mode: the previous assistant message was truncated") {
		t.Fatalf("continuation guidance was not attached; body=%s", captured)
	}
}

func TestLooksLikeClaudeCodeKiroRequestRecognizesTaskTools(t *testing.T) {
	tools := gjson.Parse(`[
		{"name":"Task","description":"Launch a subagent"},
		{"name":"AskUserQuestion","description":"Ask the user"}
	]`)
	if !looksLikeClaudeCodeKiroRequest(tools) {
		t.Fatalf("expected Claude Code Task tools to be recognized")
	}
}

func TestKiroExecutorExecuteStreamRetriesMultipleMalformedBeforePayload(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if calls.Add(1) <= 2 {
			_, _ = w.Write([]byte(`binary{"content":"oops"`))
			return
		}
		_, _ = w.Write([]byte(`binary{"content":"retry eventually ok"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-stream-retry-multiple-malformed",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-retry-multiple",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}]}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error after retries: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls.Load())
	}
	if !strings.Contains(got.String(), "retry eventually ok") {
		t.Fatalf("retry response was not streamed to client: %s", got.String())
	}
}

func TestKiroExecutorExecuteStreamRetriesClaudeCodeIntentionPreambleWithInvalidToolBeforePayload(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`binary{"content":"我"}` +
				`binary{"content":"来探索工作区的各个子项目模块。"}` +
				`binary{"name":"Agent","toolUseId":"bad-agent","input":"{\"prompt\":\"read project","stop":false}`))
			return
		}
		_, _ = w.Write([]byte(`binary{"content":"已完成模块梳理。"}`))
	}))
	defer server.Close()

	origTemplate := helps.KiroBaseURLTemplate
	helps.KiroBaseURLTemplate = server.URL
	defer func() { helps.KiroBaseURLTemplate = origTemplate }()

	executor := NewKiroExecutor(nil)
	auth := &cliproxyauth.Auth{
		ID:       "kiro-stream-retry-intention-invalid-tool",
		Provider: "kiro",
		Metadata: map[string]any{
			"accessToken": "at-stream-retry-intention",
			"region":      "us-east-1",
		},
	}
	payload := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"explore"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"X-Anthropic-Billing-Header": []string{"cc_version=2.1.143; cc_entrypoint=sdk-cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error after retry: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if !strings.Contains(got.String(), "已完成模块梳理") {
		t.Fatalf("retry response was not streamed to client: %s", got.String())
	}
	if strings.Contains(got.String(), "我来探索") {
		t.Fatalf("intention-only preamble leaked to client: %s", got.String())
	}
}

func TestKiroExecutorCountTokensKeepsUnsupported(t *testing.T) {
	executor := NewKiroExecutor(nil)
	_, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{Model: "claude-sonnet-4-5"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("CountTokens error = nil, want unsupported")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("CountTokens error = %T %v, want 501 status", err, err)
	}
	if !strings.Contains(err.Error(), "cpa_kiro_count_tokens_unsupported") {
		t.Fatalf("CountTokens error = %q, want cpa unsupported marker", err.Error())
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
		// NOTE: cases targeting `claude-sonnet-4-5` exercise the pipeline's
		// kiro-specific budget clamp (the kiro registry entry caps thinking
		// at 24576). The executor's tier policy
		// (TestKiroEnabledOnLighterTierDropsThinking) then suppresses the
		// generated prefix on those lighter tiers, so wantThinkingPrefix is
		// false even when wantThinkingType=="enabled". Cases targeting
		// `claude-sonnet-4-6` (which supports adaptive levels) exercise the
		// `enabled+budget → adaptive+effort` rewrite and keep
		// wantThinkingPrefix=true.
		{
			name:               "suffix budget override clamps and suppresses on lighter tier",
			model:              "claude-sonnet-4-5(8192)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "enabled",
			wantBudgetTokens:   8192,
			wantThinkingPrefix: false,
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
			name:               "body enabled+budget on supports-levels tier rewrites to adaptive",
			model:              "claude-sonnet-4-6",
			body:               `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":4096}}`,
			wantThinkingType:   "enabled",
			wantBudgetTokens:   4096,
			wantThinkingPrefix: true,
		},
		{
			name:               "claude code default budget clamps to kiro max (lighter tier suppresses)",
			model:              "claude-sonnet-4-5",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`,
			wantThinkingType:   "enabled",
			wantMaxBudget:      24576,
			wantThinkingPrefix: false,
		},
		{
			name:               "no thinking config passthrough",
			model:              "claude-sonnet-4-5",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "",
			wantThinkingPrefix: false,
		},
		{
			name:               "budget clamped to model max (lighter tier suppresses)",
			model:              "claude-sonnet-4-5(99999)",
			body:               `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`,
			wantThinkingType:   "enabled",
			wantMaxBudget:      24576,
			wantThinkingPrefix: false,
		},
		{
			name:               "suffix auto enables thinking on supports-levels tier",
			model:              "claude-sonnet-4-6(auto)",
			body:               `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`,
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
			// Default effort "" exercises the medium fallback so this test reflects
			// production behavior when KiroConfig is unset.
			cwReq, _, errBuild := buildKiroCodeWhispererRequest(processed, nil, "")
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
// toolUse events with the same toolUseID emit only one content_block_start, one
// valid accumulated input_json_delta, and exactly one content_block_stop. The
// executor buffers Kiro tool input shards until they form complete JSON so
// Claude Code never executes a half-written tool input as `{}`.
func TestStreamKiroToClaudeSSE_SameToolUseID_MultiEvent(t *testing.T) {
	raw := `{"content": "Let me check."}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "{\"loc\":", "stop": false}` +
		`binary{"name": "get_weather", "toolUseId": "tu-dup", "input": "\"NYC\"}", "stop": true}`
	reader := strings.NewReader(raw)
	var lines []string
	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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
	if inputDeltas != 1 {
		t.Errorf("expected exactly 1 accumulated input_json_delta event, got %d", inputDeltas)
	}
	// Verify both upstream shards were preserved inside the accumulated delta.
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
	if len(shards) != 1 {
		t.Fatalf("expected exactly 1 input_json_delta shard, got %d", len(shards))
	}
	if !strings.Contains(shards[0], `loc`) {
		t.Errorf("accumulated shard should contain initial input fragment, got: %s", shards[0])
	}
	if !strings.Contains(shards[0], `NYC`) {
		t.Errorf("accumulated shard should contain continuation input, got: %s", shards[0])
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
	_, _ = streamKiroToClaudeSSE(nil, reader, nil, "claude-sonnet-4-5", func(line []byte) {
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
	// Default effort "" exercises the medium fallback for enabled+budget rewrites,
	// but explicit adaptive+high from the user must still win and be preserved.
	cwReq, _, errBuild := buildKiroCodeWhispererRequest(processed, nil, "")
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

// TestKiroEnabledBudgetRewriteToAdaptive guards the Kiro Opus 4.6 reliability
// fix: client `thinking.type=enabled` requests (the shape Claude Code sends by
// default) must be rewritten to Kiro's `adaptive` thinking mode at the
// configured effort, instead of being forwarded as `enabled+budget` which
// triggers the no-visible-text upstream regression in 25-50% of measured
// requests. Explicit `(none)` or `(high)` user choices must not be touched.
func TestKiroEnabledBudgetRewriteToAdaptive(t *testing.T) {
	tests := []struct {
		name           string
		thinkingJSON   string
		defaultEffort  string
		wantMode       string // "adaptive", "enabled", or "" (no prefix)
		wantEffort     string // when wantMode==adaptive
		wantBudget     int    // when wantMode==enabled
		forbidContains []string
	}{
		{
			name:          "claude code default budget rewrites to adaptive medium",
			thinkingJSON:  `{"type":"enabled","budget_tokens":31999}`,
			defaultEffort: "",
			wantMode:      "adaptive",
			wantEffort:    "medium",
			forbidContains: []string{
				// The legacy enabled+max_thinking_length path must not appear,
				// otherwise the high-budget no-output failure mode is reachable.
				"max_thinking_length",
			},
		},
		{
			name:          "operator override to high is honored",
			thinkingJSON:  `{"type":"enabled","budget_tokens":24576}`,
			defaultEffort: "high",
			wantMode:      "adaptive",
			wantEffort:    "high",
		},
		{
			name:          "operator preserve keeps enabled+budget verbatim",
			thinkingJSON:  `{"type":"enabled","budget_tokens":24576}`,
			defaultEffort: "preserve",
			wantMode:      "enabled",
			wantBudget:    24576,
		},
		{
			name:          "explicit adaptive low always wins over default high",
			thinkingJSON:  `{"type":"adaptive"}`,
			defaultEffort: "high",
			wantMode:      "adaptive",
			wantEffort:    "low",
			// Explicit user choice arrives via output_config.effort; injected below.
		},
		{
			name:          "disabled remains disabled",
			thinkingJSON:  `{"type":"disabled"}`,
			defaultEffort: "medium",
			wantMode:      "",
			forbidContains: []string{
				"thinking_mode",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-4-6","messages":[{"role":"user","content":"hi"}],"thinking":` + tt.thinkingJSON + `}`)
			// For the "explicit adaptive" case, inject output_config.effort=low so
			// the executor reads it from the canonical location.
			if tt.name == "explicit adaptive low always wins over default high" {
				var err error
				body, err = sjson.SetBytes(body, "output_config.effort", "low")
				if err != nil {
					t.Fatalf("sjson.SetBytes: %v", err)
				}
			}

			cwReq, _, errBuild := buildKiroCodeWhispererRequest(body, nil, tt.defaultEffort)
			if errBuild != nil {
				t.Fatalf("buildKiroCodeWhispererRequest error: %v", errBuild)
			}
			// The prompt prefix is embedded as JSON-encoded text inside the
			// userInputMessage.content field. Decode it before asserting on
			// the literal `<thinking_mode>` markup we care about.
			content := gjson.GetBytes(cwReq, "conversationState.currentMessage.userInputMessage.content").String()

			for _, forbidden := range tt.forbidContains {
				if strings.Contains(content, forbidden) {
					t.Errorf("prompt must not contain %q; content=%q", forbidden, content)
				}
			}

			switch tt.wantMode {
			case "":
				if strings.Contains(content, "thinking_mode") {
					t.Errorf("expected no thinking prefix; content=%q", content)
				}
			case "adaptive":
				if !strings.Contains(content, "<thinking_mode>adaptive</thinking_mode>") {
					t.Errorf("expected adaptive mode tag; content=%q", content)
				}
				if tt.wantEffort != "" {
					want := "<thinking_effort>" + tt.wantEffort + "</thinking_effort>"
					if !strings.Contains(content, want) {
						t.Errorf("expected effort %q; content=%q", tt.wantEffort, content)
					}
				}
			case "enabled":
				if !strings.Contains(content, "<thinking_mode>enabled</thinking_mode>") {
					t.Errorf("expected enabled mode tag; content=%q", content)
				}
				if tt.wantBudget > 0 {
					want := fmt.Sprintf("<max_thinking_length>%d</max_thinking_length>", tt.wantBudget)
					if !strings.Contains(content, want) {
						t.Errorf("expected budget %d preserved; content=%q", tt.wantBudget, content)
					}
				}
			default:
				t.Fatalf("unsupported wantMode %q", tt.wantMode)
			}
		})
	}
}

// TestKiroEnabledOnLighterTierDropsThinking guards the tier-aware policy in
// buildKiroCodeWhispererRequest: Claude Code's default `enabled+budget`
// thinking shape must be SUPPRESSED entirely on lighter Kiro tiers
// (haiku 4.5, sonnet 4.5, opus 4.5) that do not understand
// <thinking_mode>adaptive</thinking_mode>+<thinking_effort>...</thinking_effort>.
//
// Without this policy the proxy's default `enabled+budget → adaptive+medium`
// rewrite would still fire on lighter tiers, sending an upstream-ignored
// adaptive prefix and adding latency to Claude Code's auto-routed light-tier
// traffic (completion summary, telemetry, internal subagent calls).
//
// Heavier tiers (sonnet 4.6, opus 4.6, opus 4.7) must still receive the
// rewrite, since that is where the fluency bug being mitigated lives.
func TestKiroEnabledOnLighterTierDropsThinking(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		thinkingJSON   string
		wantPrefix     bool
		mustContain    string
		mustNotContain []string
	}{
		{
			name:           "haiku-4-5 enabled+budget: thinking suppressed",
			model:          "claude-haiku-4-5",
			thinkingJSON:   `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:     false,
			mustNotContain: []string{"thinking_mode", "thinking_effort", "max_thinking_length"},
		},
		{
			name:         "haiku dated auto-route alias enabled+budget: upgraded to sonnet adaptive",
			model:        "claude-haiku-4-5-20251001",
			thinkingJSON: `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:   true,
			mustContain:  "<thinking_mode>adaptive</thinking_mode>",
		},
		{
			name:           "sonnet-4-5 enabled+budget: thinking suppressed",
			model:          "claude-sonnet-4-5",
			thinkingJSON:   `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:     false,
			mustNotContain: []string{"thinking_mode", "thinking_effort", "max_thinking_length"},
		},
		{
			name:           "opus-4-5 enabled+budget: thinking suppressed",
			model:          "claude-opus-4-5",
			thinkingJSON:   `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:     false,
			mustNotContain: []string{"thinking_mode", "thinking_effort", "max_thinking_length"},
		},
		{
			name:         "sonnet-4-6 (supports levels) enabled+budget: still rewritten to adaptive+medium",
			model:        "claude-sonnet-4-6",
			thinkingJSON: `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:   true,
			mustContain:  "<thinking_mode>adaptive</thinking_mode>",
		},
		{
			name:         "opus-4-6 (supports levels) enabled+budget: still rewritten to adaptive+medium",
			model:        "claude-opus-4-6",
			thinkingJSON: `{"type":"enabled","budget_tokens":31999}`,
			wantPrefix:   true,
			mustContain:  "<thinking_mode>adaptive</thinking_mode>",
		},
		{
			// Explicit adaptive must still pass through on lighter tiers, so
			// callers that opt in deliberately are not silently dropped.
			name:         "haiku explicit adaptive: prefix preserved",
			model:        "claude-haiku-4-5",
			thinkingJSON: `{"type":"adaptive"}`,
			wantPrefix:   true,
			mustContain:  "<thinking_mode>adaptive</thinking_mode>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(
				`{"model":%q,"messages":[{"role":"user","content":"hi"}],"thinking":%s}`,
				tt.model, tt.thinkingJSON,
			))
			cwReq, _, err := buildKiroCodeWhispererRequest(body, nil, "medium")
			if err != nil {
				t.Fatalf("buildKiroCodeWhispererRequest error: %v", err)
			}
			content := gjson.GetBytes(cwReq, "conversationState.currentMessage.userInputMessage.content").String()

			for _, forbid := range tt.mustNotContain {
				if strings.Contains(content, forbid) {
					t.Errorf("content must not contain %q; got %q", forbid, content)
				}
			}
			if tt.wantPrefix {
				if tt.mustContain != "" && !strings.Contains(content, tt.mustContain) {
					t.Errorf("content must contain %q; got %q", tt.mustContain, content)
				}
			}
		})
	}
}

// TestKiroExecutor_HttpClientForRoutesProxyVsShared verifies the keep-alive
// optimization's selector logic: requests without a per-auth/global proxy and
// without a context-injected RoundTripper must use the shared singleton client
// (so idle connections to the AWS CodeWhisperer endpoint can be pooled across
// requests). Requests with a proxy URL or a custom RoundTripper must still go
// through the legacy NewProxyAwareHTTPClient path so users relying on those
// features see no behavior change.
func TestKiroExecutor_HttpClientForRoutesProxyVsShared(t *testing.T) {
	shared := helps.KiroSharedHTTPClient()
	t.Run("default path returns shared singleton (keep-alive enabled)", func(t *testing.T) {
		e := NewKiroExecutor(nil)
		got := e.httpClientFor(context.Background(), &cliproxyauth.Auth{Provider: "kiro"})
		if got != shared {
			t.Errorf("expected shared singleton client, got distinct pointer")
		}
	})
	t.Run("auth proxy URL forces fresh transport", func(t *testing.T) {
		e := NewKiroExecutor(nil)
		got := e.httpClientFor(context.Background(), &cliproxyauth.Auth{Provider: "kiro", ProxyURL: "http://127.0.0.1:1"})
		if got == shared {
			t.Errorf("auth proxy URL must bypass shared client to honor per-auth proxy routing")
		}
	})
	t.Run("global proxy URL forces fresh transport", func(t *testing.T) {
		e := NewKiroExecutor(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://127.0.0.1:1"}})
		got := e.httpClientFor(context.Background(), &cliproxyauth.Auth{Provider: "kiro"})
		if got == shared {
			t.Errorf("global proxy URL must bypass shared client to honor cfg.ProxyURL")
		}
	})
	t.Run("context RoundTripper is honored", func(t *testing.T) {
		e := NewKiroExecutor(nil)
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", http.RoundTripper(http.DefaultTransport))
		got := e.httpClientFor(ctx, &cliproxyauth.Auth{Provider: "kiro"})
		if got == shared {
			t.Errorf("context-injected RoundTripper must bypass shared client so callers can override transport")
		}
	})
}

// TestKiroExecutor_DefaultThinkingEffortFromConfig verifies that the executor
// pulls the configured default effort from cfg.Kiro and forwards it to the
// builder, so operators can switch the rewrite target via YAML alone.
func TestKiroExecutor_DefaultThinkingEffortFromConfig(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{name: "nil cfg returns empty (helper falls back to medium)", cfg: nil, want: ""},
		{name: "empty cfg returns empty", cfg: &config.Config{}, want: ""},
		{name: "configured high", cfg: &config.Config{Kiro: config.KiroConfig{DefaultThinkingEffort: "high"}}, want: "high"},
		{name: "configured preserve", cfg: &config.Config{Kiro: config.KiroConfig{DefaultThinkingEffort: "preserve"}}, want: "preserve"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := NewKiroExecutor(tt.cfg)
			if got := e.kiroDefaultThinkingEffort(); got != tt.want {
				t.Errorf("kiroDefaultThinkingEffort() = %q, want %q", got, tt.want)
			}
		})
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

// --- HIGH-2 (P0-2 follow-up) manager persistence regression ---
//
// recordingKiroStore is a minimal in-memory cliproxyauth.Store used to verify
// that Manager.Update writes refreshed Kiro auth back to persistent storage
// after a bounded 401 -> refresh -> retry succeeds. It records every Save call
// so the test can assert exactly which credential snapshot was persisted.
type recordingKiroStore struct {
	mu    sync.Mutex
	saved []*cliproxyauth.Auth
	items map[string]*cliproxyauth.Auth
}

func newRecordingKiroStore() *recordingKiroStore {
	return &recordingKiroStore{items: make(map[string]*cliproxyauth.Auth)}
}

func (s *recordingKiroStore) List(_ context.Context) ([]*cliproxyauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*cliproxyauth.Auth, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a.Clone())
	}
	return out, nil
}

func (s *recordingKiroStore) Save(_ context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := auth.Clone()
	s.items[auth.ID] = clone
	s.saved = append(s.saved, clone)
	return auth.ID, nil
}

func (s *recordingKiroStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
	return nil
}

func (s *recordingKiroStore) latestAccessToken(authID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.saved) - 1; i >= 0; i-- {
		if s.saved[i].ID == authID {
			if v, ok := s.saved[i].Metadata["accessToken"].(string); ok {
				return v
			}
			return ""
		}
	}
	return ""
}

func (s *recordingKiroStore) saveCallsForAccessToken(authID, token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, snap := range s.saved {
		if snap.ID != authID {
			continue
		}
		if v, _ := snap.Metadata["accessToken"].(string); v == token {
			count++
		}
	}
	return count
}

// TestKiroManagerExecute_401RefreshPersistsToManagerAndStore is the regression
// test for HIGH-2 from the 2026-05-14 Kiro reliability code review. It runs a
// full Manager.Execute loop end-to-end against a scripted Kiro upstream (401
// then refresh then 200) and asserts that:
//
//  1. Execute succeeds after the bounded refresh-and-retry.
//  2. Manager.GetByID(authID) reflects the new accessToken (i.e. the in-memory
//     map was rewritten via Manager.Update, not just the local executor clone).
//  3. The configured Store received a Save call carrying the new accessToken,
//     so subsequent process restarts also start with the rotated credential.
//
// Without the HIGH-2 fix, only the executor's request-local auth clone would
// observe at-new and steps 2/3 would fail, leaving the next inbound request
// to repeat the 401 -> refresh -> retry loop on every call.
func TestKiroManagerExecute_401RefreshPersistsToManagerAndStore(t *testing.T) {
	const (
		provider = "kiro"
		model    = "claude-sonnet-4-5"
		authID   = "kiro-mgr-401-persist"
	)

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

	store := newRecordingKiroStore()
	mgr := cliproxyauth.NewManager(store, nil, nil)
	mgr.RegisterExecutor(NewKiroExecutor(nil))

	auth := newKiroAuth(authID, "at-old")
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if got := store.latestAccessToken(authID); got != "at-old" {
		t.Fatalf("store accessToken after Register = %q, want at-old (Register persisted the initial credential)", got)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	mgr.RefreshSchedulerEntry(authID)

	resp, err := mgr.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Manager.Execute err = %v, want nil after 401 -> refresh -> 200", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "after-refresh" {
		t.Fatalf("response content = %q, want after-refresh; payload=%s", got, string(resp.Payload))
	}
	if atomic.LoadInt32(&script.mainCalls) != 2 {
		t.Fatalf("main calls = %d, want 2 (initial + 1 retry)", script.mainCalls)
	}
	if atomic.LoadInt32(&script.refreshCalls) != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", script.refreshCalls)
	}

	// Manager-side assertion: the in-memory snapshot must reflect the new token.
	managerAuth, ok := mgr.GetByID(authID)
	if !ok || managerAuth == nil {
		t.Fatalf("Manager.GetByID(%q) ok=%v, auth=%v", authID, ok, managerAuth)
	}
	if got := kiroMetaStr(managerAuth, "accessToken"); got != "at-new" {
		t.Fatalf("manager accessToken = %q, want at-new (HIGH-2: refresh did not persist to manager)", got)
	}
	if got := kiroMetaStr(managerAuth, "refreshToken"); got != "rt-rotated" {
		t.Fatalf("manager refreshToken = %q, want rt-rotated (rotation must persist alongside accessToken)", got)
	}

	// Store-side assertion: rotated credential must be flushed to the backing
	// Store so the next process restart starts with the new token.
	if got := store.latestAccessToken(authID); got != "at-new" {
		t.Fatalf("store accessToken = %q, want at-new (HIGH-2: refresh did not persist to store)", got)
	}
	if got := store.saveCallsForAccessToken(authID, "at-new"); got < 1 {
		t.Fatalf("store recorded %d Save call(s) carrying at-new, want >=1", got)
	}
}

// TestKiroManagerExecute_401RefreshNextRequestUsesNewToken extends the HIGH-2
// regression test by issuing a second independent Manager.Execute after the
// refresh-retry round trip and asserting that the next request goes out with
// at-new on the very first try (no second 401, no second refresh). This is the
// concrete user-visible symptom HIGH-2 was about: without the fix, every
// inbound request would repeat the 401 -> refresh -> retry loop because the
// manager kept handing out the stale at-old token.
func TestKiroManagerExecute_401RefreshNextRequestUsesNewToken(t *testing.T) {
	const (
		provider = "kiro"
		model    = "claude-sonnet-4-5"
		authID   = "kiro-mgr-401-next-request"
	)

	script := &kiroScript{
		t: t,
		mainResponses: []func(http.ResponseWriter, *http.Request){
			errorKiroResponse(http.StatusUnauthorized, `{"error":"expired"}`, nil),
			func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
					t.Fatalf("retry Authorization = %q, want Bearer at-new", got)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(`binary{"content":"first-after-refresh"}`))
			},
			// Second inbound request must succeed on first try with at-new.
			func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
					t.Fatalf("second request Authorization = %q, want Bearer at-new (HIGH-2 regression: manager handed out stale at-old)", got)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				_, _ = w.Write([]byte(`binary{"content":"second-with-new-token"}`))
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

	store := newRecordingKiroStore()
	mgr := cliproxyauth.NewManager(store, nil, nil)
	mgr.RegisterExecutor(NewKiroExecutor(nil))

	auth := newKiroAuth(authID, "at-old")
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	mgr.RefreshSchedulerEntry(authID)

	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := mgr.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{
		Model:   model,
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("first Manager.Execute err = %v, want success after refresh", err)
	}
	if got := atomic.LoadInt32(&script.refreshCalls); got != 1 {
		t.Fatalf("refresh calls after first request = %d, want 1", got)
	}

	// Now run a second request. With the fix it must succeed on the first try
	// and must NOT trigger another refresh.
	resp, err := mgr.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{
		Model:   model,
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("second Manager.Execute err = %v, want success on first try with persisted token", err)
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "second-with-new-token" {
		t.Fatalf("second response content = %q, want second-with-new-token", got)
	}

	// Hard guarantees:
	// - Total main calls = 3 (1 initial 401 + 1 retry after refresh + 1 fresh
	//   request that must succeed without retry).
	// - Total refresh calls = 1 (no extra refresh for the second request).
	if got := atomic.LoadInt32(&script.mainCalls); got != 3 {
		t.Fatalf("total main calls = %d, want 3 (1 initial 401 + 1 retry + 1 fresh success)", got)
	}
	if got := atomic.LoadInt32(&script.refreshCalls); got != 1 {
		t.Fatalf("total refresh calls = %d, want 1 (HIGH-2: stale manager token would force a 2nd refresh)", got)
	}
}

// --- Kiro thinking streaming state-machine regression coverage ---
//
// The following tests pin down the contract that Kiro `<thinking>...</thinking>`
// markup must never leak into Claude `text_delta` events, even when the tags
// (or their bodies) are split across multiple Kiro `content` events. They also
// verify that history rebuilders strip already-leaked thinking markup so that
// multi-turn conversations do not keep poisoning the Kiro context window.

// streamSSESummary parses an SSE byte stream emitted by streamKiroToClaudeSSE
// into a flat representation that is easy to assert against.
type streamSSESummary struct {
	textDeltas     []string
	thinkingDeltas []string
	blockStarts    []string // content_block_start "type" values, in order
	blockStops     int
	hasMessageStop bool
}

func summarizeKiroSSELines(lines []string) streamSSESummary {
	var s streamSSESummary
	for _, line := range lines {
		for _, dataLine := range strings.Split(line, "\n") {
			dataLine = strings.TrimSpace(dataLine)
			if !strings.HasPrefix(dataLine, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(dataLine, "data:"))
			eventType := gjson.Get(payload, "type").String()
			switch eventType {
			case "content_block_start":
				s.blockStarts = append(s.blockStarts, gjson.Get(payload, "content_block.type").String())
			case "content_block_delta":
				deltaType := gjson.Get(payload, "delta.type").String()
				switch deltaType {
				case "text_delta":
					s.textDeltas = append(s.textDeltas, gjson.Get(payload, "delta.text").String())
				case "thinking_delta":
					s.thinkingDeltas = append(s.thinkingDeltas, gjson.Get(payload, "delta.thinking").String())
				}
			case "content_block_stop":
				s.blockStops++
			case "message_stop":
				s.hasMessageStop = true
			}
		}
	}
	return s
}

func runKiroStreamSSE(t *testing.T, raw string) streamSSESummary {
	t.Helper()
	var lines []string
	result, err := streamKiroToClaudeSSE(context.Background(), strings.NewReader(raw), nil, "claude-sonnet-4-5", func(line []byte) {
		lines = append(lines, string(line))
	})
	if err != nil {
		t.Fatalf("streamKiroToClaudeSSE error = %v, want nil", err)
	}
	if !result.payloadStarted {
		t.Fatalf("payloadStarted = false, want true")
	}
	return summarizeKiroSSELines(lines)
}

// TestStreamKiroToClaudeSSE_ThinkingStartTagSplitAcrossEvents verifies that a
// `<thinking>` opening tag arriving in one Kiro `content` event followed by the
// reasoning body and `</thinking>` in subsequent events is rendered as a single
// thinking block instead of leaking the body and closing tag as text_delta.
func TestStreamKiroToClaudeSSE_ThinkingStartTagSplitAcrossEvents(t *testing.T) {
	raw := `binary{"content":"<thinking>"}` +
		`binary{"content":"Internal reasoning here."}` +
		`binary{"content":"</thinking>\n\nFinal answer."}`

	summary := runKiroStreamSSE(t, raw)

	joinedText := strings.Join(summary.textDeltas, "")
	if strings.Contains(joinedText, "<thinking>") || strings.Contains(joinedText, "</thinking>") {
		t.Fatalf("text_delta leaked thinking markup: %q", joinedText)
	}
	if strings.Contains(joinedText, "Internal reasoning here.") {
		t.Fatalf("text_delta leaked thinking body: %q", joinedText)
	}
	if joinedText != "Final answer." {
		t.Fatalf("text_delta content = %q, want %q", joinedText, "Final answer.")
	}

	joinedThinking := strings.Join(summary.thinkingDeltas, "")
	if !strings.Contains(joinedThinking, "Internal reasoning here.") {
		t.Fatalf("thinking_delta missing reasoning body: %q", joinedThinking)
	}
	if strings.Contains(joinedThinking, "<thinking>") || strings.Contains(joinedThinking, "</thinking>") {
		t.Fatalf("thinking_delta should not contain raw tags: %q", joinedThinking)
	}

	thinkingStarts := 0
	for _, blockType := range summary.blockStarts {
		if blockType == "thinking" {
			thinkingStarts++
		}
	}
	if thinkingStarts != 1 {
		t.Fatalf("expected exactly 1 thinking content_block_start, got %d (starts=%v)", thinkingStarts, summary.blockStarts)
	}
}

// TestStreamKiroToClaudeSSE_ThinkingBodyAndCloseSplit verifies the case where
// the start tag and part of the body share an event but the closing tag and the
// final answer arrive in a later event. Kiro upstream commonly chunks like this.
func TestStreamKiroToClaudeSSE_ThinkingBodyAndCloseSplit(t *testing.T) {
	raw := `binary{"content":"<thinking>\nLet me think"}` +
		`binary{"content":" carefully.</thinking>\n\nReady."}`

	summary := runKiroStreamSSE(t, raw)

	joinedText := strings.Join(summary.textDeltas, "")
	if strings.Contains(joinedText, "</thinking>") || strings.Contains(joinedText, "Let me think") {
		t.Fatalf("text_delta leaked thinking content: %q", joinedText)
	}
	if joinedText != "Ready." {
		t.Fatalf("text_delta content = %q, want %q", joinedText, "Ready.")
	}

	joinedThinking := strings.Join(summary.thinkingDeltas, "")
	if !strings.Contains(joinedThinking, "Let me think") || !strings.Contains(joinedThinking, "carefully.") {
		t.Fatalf("thinking_delta should contain both halves of reasoning: %q", joinedThinking)
	}
}

// TestStreamKiroToClaudeSSE_ThinkingClosesInSameEventWithSuffix verifies the
// happy path where a single Kiro content event carries the entire
// `<thinking>...</thinking>` block plus a visible suffix.
func TestStreamKiroToClaudeSSE_ThinkingClosesInSameEventWithSuffix(t *testing.T) {
	raw := `binary{"content":"<thinking>quick thought</thinking>\n\nDone."}`

	summary := runKiroStreamSSE(t, raw)

	joinedText := strings.Join(summary.textDeltas, "")
	if joinedText != "Done." {
		t.Fatalf("text_delta content = %q, want %q", joinedText, "Done.")
	}
	if got := strings.Join(summary.thinkingDeltas, ""); got != "quick thought" {
		t.Fatalf("thinking_delta = %q, want %q", got, "quick thought")
	}
}

// TestStreamKiroToClaudeSSE_ThinkingUnclosedAtEOFDoesNotLeak verifies that when
// the upstream stream ends in the middle of a thinking block, the partial
// reasoning is not promoted to a visible text_delta. The proxy may flush it as
// thinking_delta (or drop it entirely), but it must never reach the user.
func TestStreamKiroToClaudeSSE_ThinkingUnclosedAtEOFDoesNotLeak(t *testing.T) {
	raw := `binary{"content":"prefix <thinking>"}` +
		`binary{"content":"never closed reasoning"}`

	summary := runKiroStreamSSE(t, raw)

	joinedText := strings.Join(summary.textDeltas, "")
	if strings.Contains(joinedText, "<thinking>") || strings.Contains(joinedText, "never closed reasoning") {
		t.Fatalf("text_delta leaked thinking content at EOF: %q", joinedText)
	}
	if joinedText != "prefix " {
		t.Fatalf("text_delta content = %q, want only the prefix before the open tag", joinedText)
	}
	if !summary.hasMessageStop {
		t.Fatalf("stream should still emit message_stop on EOF with unclosed thinking")
	}
}

// TestStreamKiroToClaudeSSE_ThinkingFollowedByToolUse verifies that thinking
// content does not interfere with subsequent tool_use blocks. The thinking
// block must be properly stopped before the tool block starts and tool input
// must reach the client untouched.
func TestStreamKiroToClaudeSSE_ThinkingFollowedByToolUse(t *testing.T) {
	raw := `binary{"content":"<thinking>plan the call</thinking>"}` +
		`binary{"name":"bash","toolUseId":"tu-x","input":"{\"cmd\":\"ls\"}","stop":true}`

	summary := runKiroStreamSSE(t, raw)

	joinedText := strings.Join(summary.textDeltas, "")
	if strings.Contains(joinedText, "<thinking>") || strings.Contains(joinedText, "</thinking>") {
		t.Fatalf("text_delta leaked thinking markup near tool_use: %q", joinedText)
	}

	if got := strings.Join(summary.thinkingDeltas, ""); got != "plan the call" {
		t.Fatalf("thinking_delta = %q, want %q", got, "plan the call")
	}

	sawThinking := false
	sawTool := false
	for i, blockType := range summary.blockStarts {
		if blockType == "thinking" {
			sawThinking = true
		}
		if blockType == "tool_use" {
			sawTool = true
			if !sawThinking {
				t.Fatalf("tool_use block_start at index %d came before thinking block (starts=%v)", i, summary.blockStarts)
			}
		}
	}
	if !sawThinking || !sawTool {
		t.Fatalf("expected both thinking and tool_use content blocks, got starts=%v", summary.blockStarts)
	}
}

// TestBuildKiroAssistantHistoryMessage_StripsLeakedThinkingFromText verifies
// that when a previous assistant turn (as serialized by Claude Code) contains
// a `text` block with leaked `<thinking>...</thinking>` markup, the Kiro
// history builder hides that markup from the upstream Kiro context. The bug we
// are guarding against is: leaked reasoning gets re-sent as plain assistant
// content, accumulating turn after turn until the model loses focus.
func TestBuildKiroAssistantHistoryMessage_StripsLeakedThinkingFromText(t *testing.T) {
	msg := gjson.Parse(`{
		"role": "assistant",
		"content": [
			{"type": "text", "text": "Visible answer."},
			{"type": "text", "text": "<thinking>leaked reasoning</thinking>\n\nMore visible."},
			{"type": "text", "text": "tail "}
		]
	}`)

	out := buildKiroAssistantHistoryMessage(msg, helps.BuildKiroToolNameMaps(nil))
	arm, ok := out["assistantResponseMessage"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected assistantResponseMessage map, got %T", out["assistantResponseMessage"])
	}
	content, ok := arm["content"].(string)
	if !ok {
		t.Fatalf("expected string content, got %T", arm["content"])
	}

	if strings.Contains(content, "leaked reasoning") {
		t.Fatalf("history content still contains leaked reasoning body: %q", content)
	}
	if !strings.Contains(content, "Visible answer.") || !strings.Contains(content, "More visible.") || !strings.Contains(content, "tail") {
		t.Fatalf("history content lost legitimate user-visible text: %q", content)
	}

	// If the helper preserves leaked reasoning at all, it must wrap it back
	// inside the canonical thinking tag (so Kiro treats it as reasoning, not
	// as visible assistant output). It is also acceptable to drop it entirely.
	if strings.Contains(content, "<thinking>") {
		if !strings.Contains(content, "</thinking>") {
			t.Fatalf("history content has unbalanced thinking markup: %q", content)
		}
	}
}

// TestBuildKiroAssistantHistoryMessage_DropsOrphanCloseTag verifies that an
// orphan `</thinking>` tag (which can only appear because of an earlier
// streaming bug) is removed from the visible assistant content rather than
// being faithfully forwarded to Kiro.
func TestBuildKiroAssistantHistoryMessage_DropsOrphanCloseTag(t *testing.T) {
	msg := gjson.Parse(`{
		"role": "assistant",
		"content": [
			{"type": "text", "text": "ought through it.</thinking>\n\nReal answer."}
		]
	}`)

	out := buildKiroAssistantHistoryMessage(msg, helps.BuildKiroToolNameMaps(nil))
	arm := out["assistantResponseMessage"].(map[string]interface{})
	content := arm["content"].(string)

	if strings.Contains(content, "</thinking>") {
		t.Fatalf("history still carries orphan close tag: %q", content)
	}
	if strings.Contains(content, "ought through it.") {
		t.Fatalf("history kept orphan reasoning fragment: %q", content)
	}
	if !strings.Contains(content, "Real answer.") {
		t.Fatalf("history dropped legitimate assistant answer: %q", content)
	}
}
