package cliproxy

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceWarmupExecutor struct {
	provider string

	mu    sync.Mutex
	calls []serviceWarmupCall
	ch    chan serviceWarmupCall
}

type serviceWarmupCall struct {
	authID    string
	model     string
	synthetic string
	pinned    string
}

type stuckServiceWarmupExecutor struct {
	provider string
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newServiceWarmupExecutor(provider string) *serviceWarmupExecutor {
	return &serviceWarmupExecutor{
		provider: provider,
		ch:       make(chan serviceWarmupCall, 16),
	}
}

func (e *serviceWarmupExecutor) Identifier() string { return e.provider }

func (e *serviceWarmupExecutor) Execute(_ context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	call := serviceWarmupCall{
		authID: auth.ID,
		model:  req.Model,
	}
	if opts.Metadata != nil {
		if value, ok := opts.Metadata[cliproxyexecutor.SyntheticRequestMetadataKey].(string); ok {
			call.synthetic = value
		}
		if value, ok := opts.Metadata[cliproxyexecutor.PinnedAuthMetadataKey].(string); ok {
			call.pinned = value
		}
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
	select {
	case e.ch <- call:
	default:
	}
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *serviceWarmupExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *serviceWarmupExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *serviceWarmupExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *serviceWarmupExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *stuckServiceWarmupExecutor) Identifier() string { return e.provider }

func (e *stuckServiceWarmupExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.once.Do(func() {
		close(e.started)
	})
	<-e.release
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *stuckServiceWarmupExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *stuckServiceWarmupExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *stuckServiceWarmupExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *stuckServiceWarmupExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *serviceWarmupExecutor) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func TestAuthProviderWarmupRunnerRunOncePinsCodexAuthIndex(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := newServiceWarmupExecutor("codex")
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "codex-warmup-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	auth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}
	runner := newAuthProviderWarmupRunner(service)
	warmupCfg := config.AuthProviderWarmupConfig{
		Enabled:        true,
		Interval:       "1h",
		Jitter:         "0s",
		MaxConcurrency: 1,
		Prompt:         "ping",
		Providers: []config.WarmupProviderConfig{{
			Provider:    "codex",
			Enabled:     true,
			Model:       "gpt-5.3-codex",
			AuthIndexes: []string{auth.Index},
		}},
	}
	warmupCfg.Normalize()

	runner.runOnce(ctx, warmupCfg)

	select {
	case call := <-exec.ch:
		if call.authID != auth.ID {
			t.Fatalf("warmup auth ID = %q, want %q", call.authID, auth.ID)
		}
		if call.model != "gpt-5.3-codex" {
			t.Fatalf("warmup model = %q, want gpt-5.3-codex", call.model)
		}
		if call.synthetic != cliproxyexecutor.SyntheticRequestKindWarmup {
			t.Fatalf("synthetic marker = %q, want warmup", call.synthetic)
		}
		if call.pinned != auth.ID {
			t.Fatalf("pinned auth = %q, want %q", call.pinned, auth.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scheduled warmup execution")
	}

	gotAuth, ok := manager.GetByID(auth.ID)
	if !ok || gotAuth == nil {
		t.Fatalf("expected auth after warmup")
	}
	if gotAuth.Success != 0 || gotAuth.Failed != 0 {
		t.Fatalf("synthetic warmup changed auth totals: success=%d failed=%d", gotAuth.Success, gotAuth.Failed)
	}
}

func TestServiceApplyAuthProviderWarmupConfigStopsDisabledRunner(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := newServiceWarmupExecutor("codex")
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "codex-scheduled-warmup-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	auth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}
	enabledCfg := &config.Config{}
	enabledCfg.AuthProviderWarmup = config.AuthProviderWarmupConfig{
		Enabled:        true,
		Interval:       "25ms",
		Jitter:         "0s",
		MaxConcurrency: 1,
		Prompt:         "ping",
		Providers: []config.WarmupProviderConfig{{
			Provider:    "codex",
			Enabled:     true,
			Model:       "gpt-5.3-codex",
			AuthIndexes: []string{auth.Index},
		}},
	}

	service.applyAuthProviderWarmupConfig(enabledCfg)
	t.Cleanup(func() {
		service.stopAuthProviderWarmup()
	})

	select {
	case <-exec.ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for enabled scheduled warmup")
	}

	disabledCfg := &config.Config{}
	disabledCfg.AuthProviderWarmup = config.AuthProviderWarmupConfig{
		Enabled:  false,
		Interval: "25ms",
		Jitter:   "0s",
	}
	service.applyAuthProviderWarmupConfig(disabledCfg)
	baseline := exec.callCount()

	time.Sleep(100 * time.Millisecond)
	if got := exec.callCount(); got != baseline {
		t.Fatalf("warmup kept running after disable: baseline=%d got=%d", baseline, got)
	}
}

func TestServiceStopAuthProviderWarmupWithContextDoesNotWaitForStuckExecute(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := &stuckServiceWarmupExecutor{
		provider: "codex",
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "codex-stuck-warmup-auth", Provider: "codex", Status: coreauth.StatusActive}
	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	auth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		close(exec.release)
	})

	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}
	enabledCfg := &config.Config{}
	enabledCfg.AuthProviderWarmup = config.AuthProviderWarmupConfig{
		Enabled:        true,
		Interval:       "1h",
		Jitter:         "0s",
		MaxConcurrency: 1,
		Prompt:         "ping",
		Providers: []config.WarmupProviderConfig{{
			Provider:    "codex",
			Enabled:     true,
			Model:       "gpt-5.3-codex",
			AuthIndexes: []string{auth.Index},
		}},
	}

	service.applyAuthProviderWarmupConfig(enabledCfg)
	select {
	case <-exec.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stuck warmup to start")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if errStop := service.stopAuthProviderWarmupWithContext(stopCtx); errStop != nil {
		t.Fatalf("stopAuthProviderWarmupWithContext returned error: %v", errStop)
	}
}

func TestServiceAuthProviderWarmupRetriesUntilPinnedAuthExists(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := newServiceWarmupExecutor("codex")
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{ID: "codex-delayed-warmup-auth", Provider: "codex", Status: coreauth.StatusActive}
	auth.EnsureIndex()
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}
	enabledCfg := &config.Config{}
	enabledCfg.AuthProviderWarmup = config.AuthProviderWarmupConfig{
		Enabled:        true,
		Interval:       "1h",
		Jitter:         "0s",
		MaxConcurrency: 1,
		Prompt:         "ping",
		Providers: []config.WarmupProviderConfig{{
			Provider:    "codex",
			Enabled:     true,
			Model:       "gpt-5.3-codex",
			AuthIndexes: []string{auth.Index},
		}},
	}

	oldRetryInterval := authProviderWarmupRetryInterval
	authProviderWarmupRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		authProviderWarmupRetryInterval = oldRetryInterval
		service.stopAuthProviderWarmup()
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	service.applyAuthProviderWarmupConfig(enabledCfg)
	time.Sleep(30 * time.Millisecond)
	if got := exec.callCount(); got != 0 {
		t.Fatalf("warmup executed before pinned auth existed: %d", got)
	}

	if _, errRegister := manager.Register(coreauth.WithSkipPersist(ctx), auth); errRegister != nil {
		t.Fatalf("register delayed auth: %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})

	select {
	case call := <-exec.ch:
		if call.authID != auth.ID {
			t.Fatalf("warmup auth ID = %q, want %q", call.authID, auth.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delayed pinned warmup retry")
	}
}
