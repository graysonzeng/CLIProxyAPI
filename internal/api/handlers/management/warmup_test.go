package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type warmupTestExecutor struct {
	provider  string
	authID    string
	model     string
	synthetic string
	pinned    string
}

func (e *warmupTestExecutor) Identifier() string { return e.provider }

func (e *warmupTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authID = auth.ID
	e.model = req.Model
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.SyntheticRequestMetadataKey].(string); ok {
			e.synthetic = value
		}
		if value, ok := opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey].(string); ok {
			e.pinned = value
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *warmupTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *warmupTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *warmupTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *warmupTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestWarmupExecutesSyntheticPinnedAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &warmupTestExecutor{provider: "kiro"}
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "kiro-auth", Provider: "kiro", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	auth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "kiro", []*registry.ModelInfo{{
		ID:   "claude-haiku-4-5-20251001",
		Type: "kiro",
	}})
	defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)

	h := &Handler{authManager: manager}
	body := []byte(`{"provider":"kiro","model":"claude-haiku-4-5-20251001","auth_id":` + strconvQuote(auth.Index) + `}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/warmup", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Warmup(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if exec.authID != "kiro-auth" {
		t.Fatalf("executor auth ID = %q, want kiro-auth", exec.authID)
	}
	if exec.model != "claude-haiku-4-5-20251001" {
		t.Fatalf("model = %q", exec.model)
	}
	if exec.synthetic != cliproxyexecutor.SyntheticRequestKindWarmup {
		t.Fatalf("synthetic metadata = %q", exec.synthetic)
	}
	if exec.pinned != "kiro-auth" {
		t.Fatalf("pinned auth = %q, want kiro-auth", exec.pinned)
	}

	var resp warmupResponse
	if errDecode := json.Unmarshal(w.Body.Bytes(), &resp); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Status != "ok" || resp.AuthID != "kiro-auth" || resp.AuthIndex != auth.Index {
		t.Fatalf("response = %+v", resp)
	}
	if got, ok := manager.GetByID("kiro-auth"); !ok || got.Success != 0 || got.Failed != 0 {
		t.Fatalf("synthetic warmup changed auth counters: got=%+v ok=%v", got, ok)
	}
}

func TestWarmupSupportsCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &warmupTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "codex-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	auth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{
		ID:   "gpt-5.3-codex",
		Type: "openai",
	}})
	defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)

	h := &Handler{authManager: manager}
	body := []byte(`{"provider":"codex","model":"gpt-5.3-codex","auth_id":` + strconvQuote(auth.Index) + `}`)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/warmup", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Warmup(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if exec.authID != "codex-auth" {
		t.Fatalf("executor auth ID = %q, want codex-auth", exec.authID)
	}
	if exec.model != "gpt-5.3-codex" {
		t.Fatalf("model = %q", exec.model)
	}
	if exec.synthetic != cliproxyexecutor.SyntheticRequestKindWarmup {
		t.Fatalf("synthetic metadata = %q", exec.synthetic)
	}
	if exec.pinned != "codex-auth" {
		t.Fatalf("pinned auth = %q, want codex-auth", exec.pinned)
	}

	var resp warmupResponse
	if errDecode := json.Unmarshal(w.Body.Bytes(), &resp); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.Status != "ok" || resp.Provider != "codex" || resp.AuthID != "codex-auth" || resp.AuthIndex != auth.Index {
		t.Fatalf("response = %+v", resp)
	}
}

func strconvQuote(value string) string {
	b, _ := json.Marshal(value)
	return string(b)
}
