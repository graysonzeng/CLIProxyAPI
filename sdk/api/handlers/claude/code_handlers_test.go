package claude

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type delayedClaudeStreamExecutor struct {
	delay time.Duration
}

func (e *delayedClaudeStreamExecutor) Identifier() string { return "claude" }

func (e *delayedClaudeStreamExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "Execute not implemented"}
}

func (e *delayedClaudeStreamExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	_ = auth
	_ = req
	_ = opts
	ch := make(chan coreexecutor.StreamChunk, 1)
	go func() {
		defer close(ch)
		timer := time.NewTimer(e.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			ch <- coreexecutor.StreamChunk{Err: ctx.Err()}
		case <-timer.C:
			ch <- coreexecutor.StreamChunk{Payload: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")}
		}
	}()
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *delayedClaudeStreamExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *delayedClaudeStreamExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented"}
}

func (e *delayedClaudeStreamExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{
		Code:       "not_implemented",
		Message:    "HttpRequest not implemented",
		HTTPStatus: http.StatusNotImplemented,
	}
}

func TestClaudeStreamingResponseEmitsKeepAliveBeforeFirstPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&delayedClaudeStreamExecutor{delay: 1500 * time.Millisecond})
	auth := &coreauth.Auth{
		ID:       "claude-auth",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "claude@example.com"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	model := "test-claude-stream"
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1},
	}, manager)
	claudeHandler := NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.POST("/v1/messages", claudeHandler.ClaudeMessages)
	server := httptest.NewServer(router)
	defer server.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"`+model+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client.Do: %v", err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(started); elapsed >= 1300*time.Millisecond {
		t.Fatalf("response headers arrived after %s; want keep-alive before delayed payload", elapsed)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if line != ": keep-alive\n" {
		t.Fatalf("first stream line = %q, want keep-alive comment", line)
	}
}
