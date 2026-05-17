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

// TestAppendDefaultWarmupProviders pins the auto-injection contract used to
// avoid the ~800ms cold-handshake tax on the first user request after a
// server restart for providers in providersWithDefaultWarmup (currently kiro).
//
// The augmenter MUST:
//
//   - inject a default kiro entry when the operator enabled warmup and
//     registered at least one healthy kiro auth without configuring kiro
//     explicitly,
//   - leave the operator's existing kiro entry untouched (no duplicates,
//     operator's model/auth-indexes/skip flags must win),
//   - not inject anything when the feature is disabled (single switch
//     controls both startup ping and periodic ping),
//   - not inject anything when there is no healthy kiro auth registered
//     (avoids polluting logs with "no auth available" warnings on
//     non-kiro deployments).
func TestAppendDefaultWarmupProviders(t *testing.T) {
	ctx := context.Background()

	newManagerWithKiro := func(t *testing.T, status coreauth.Status) *coreauth.Manager {
		mgr := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
		auth := &coreauth.Auth{ID: "kiro-warmup-default", Provider: "kiro", Status: status}
		if _, err := mgr.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("register auth: %v", err)
		}
		auth.EnsureIndex()
		return mgr
	}

	t.Run("injects kiro default when feature enabled and kiro auth registered", func(t *testing.T) {
		mgr := newManagerWithKiro(t, coreauth.StatusActive)
		cfg := config.AuthProviderWarmupConfig{
			Enabled:        true,
			Interval:       "1h",
			MaxConcurrency: 1,
		}
		got := appendDefaultWarmupProviders(cfg, mgr)
		if len(got.Providers) != 1 {
			t.Fatalf("expected 1 injected provider, got %d", len(got.Providers))
		}
		entry := got.Providers[0]
		if entry.Provider != "kiro" || entry.Model != "claude-sonnet-4-6" {
			t.Errorf("unexpected default entry: %+v", entry)
		}
		if !entry.Enabled || !entry.SkipWhenQuotaExceeded {
			t.Errorf("default entry must be enabled and skip-when-quota-exceeded; got %+v", entry)
		}
		// Source cfg must not be mutated (we copy the slice on augment).
		if len(cfg.Providers) != 0 {
			t.Errorf("source cfg.Providers mutated: %+v", cfg.Providers)
		}
	})

	t.Run("does not duplicate when operator already configured kiro", func(t *testing.T) {
		mgr := newManagerWithKiro(t, coreauth.StatusActive)
		operatorEntry := config.WarmupProviderConfig{
			Provider:    "kiro",
			Enabled:     true,
			Model:       "claude-opus-4-6",
			AuthIndexes: []string{"some-explicit-index"},
		}
		cfg := config.AuthProviderWarmupConfig{
			Enabled:   true,
			Providers: []config.WarmupProviderConfig{operatorEntry},
		}
		got := appendDefaultWarmupProviders(cfg, mgr)
		if len(got.Providers) != 1 {
			t.Fatalf("expected operator entry preserved without duplication, got %d entries", len(got.Providers))
		}
		if got.Providers[0].Model != "claude-opus-4-6" {
			t.Errorf("operator's model overwritten: %+v", got.Providers[0])
		}
	})

	t.Run("does not inject when feature disabled", func(t *testing.T) {
		mgr := newManagerWithKiro(t, coreauth.StatusActive)
		cfg := config.AuthProviderWarmupConfig{Enabled: false}
		got := appendDefaultWarmupProviders(cfg, mgr)
		if len(got.Providers) != 0 {
			t.Errorf("expected no injection when disabled, got %+v", got.Providers)
		}
	})

	t.Run("does not inject when no kiro auth registered", func(t *testing.T) {
		mgr := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
		// Register a non-kiro auth so manager is non-empty but lacks the
		// targeted provider.
		other := &coreauth.Auth{ID: "codex-x", Provider: "codex", Status: coreauth.StatusActive}
		if _, err := mgr.Register(coreauth.WithSkipPersist(ctx), other); err != nil {
			t.Fatalf("register other auth: %v", err)
		}
		cfg := config.AuthProviderWarmupConfig{Enabled: true}
		got := appendDefaultWarmupProviders(cfg, mgr)
		if len(got.Providers) != 0 {
			t.Errorf("expected no injection without kiro auth, got %+v", got.Providers)
		}
	})

	t.Run("ignores disabled kiro auth", func(t *testing.T) {
		// Disabled auths must NOT count as "healthy" for the purposes of
		// auto-injection, otherwise we'd warm a connection pool for an auth
		// that the executor will refuse to use.
		mgr := newManagerWithKiro(t, coreauth.StatusDisabled)
		cfg := config.AuthProviderWarmupConfig{Enabled: true}
		got := appendDefaultWarmupProviders(cfg, mgr)
		if len(got.Providers) != 0 {
			t.Errorf("expected no injection when only kiro auth is disabled, got %+v", got.Providers)
		}
	})

	t.Run("nil manager returns cfg unchanged", func(t *testing.T) {
		cfg := config.AuthProviderWarmupConfig{Enabled: true}
		got := appendDefaultWarmupProviders(cfg, nil)
		if len(got.Providers) != 0 {
			t.Errorf("expected nil-manager noop, got %+v", got.Providers)
		}
	})
}

// TestRunProviderWarmsAllHealthyAuthsWhenIndexesEmpty pins the multi-account
// warmup contract: when the operator did NOT list explicit auth-indexes for
// a provider, runProvider must enumerate every healthy auth registered for
// that provider and warm each one (pinned). Without this pass a multi-codex
// queue setup would only refresh the currently active candidate's TLS pool
// and OAuth token while standby accounts went cold and paid the
// ~800ms cold-handshake tax on the first request after fail-over.
//
// Inverse case (explicit auth-indexes) is covered by the existing
// TestAuthProviderWarmupRunnerRunOncePinsCodexAuthIndex above.
func TestRunProviderWarmsAllHealthyAuthsWhenIndexesEmpty(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	exec := newServiceWarmupExecutor("codex")
	manager.RegisterExecutor(exec)

	// Register 3 healthy codex auths + 1 disabled + 1 from a different
	// provider. Only the 3 healthy codex auths should be warmed.
	healthyIDs := []string{"codex-active-1", "codex-active-2", "codex-active-3"}
	for _, id := range healthyIDs {
		auth := &coreauth.Auth{ID: id, Provider: "codex", Status: coreauth.StatusActive}
		if _, err := manager.Register(coreauth.WithSkipPersist(ctx), auth); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
		auth.EnsureIndex()
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	disabledAuth := &coreauth.Auth{ID: "codex-disabled", Provider: "codex", Status: coreauth.StatusActive, Disabled: true}
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), disabledAuth); err != nil {
		t.Fatalf("register disabled: %v", err)
	}
	disabledAuth.EnsureIndex()
	registry.GetGlobalRegistry().RegisterClient(disabledAuth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(disabledAuth.ID) })

	otherAuth := &coreauth.Auth{ID: "kiro-x", Provider: "kiro", Status: coreauth.StatusActive}
	if _, err := manager.Register(coreauth.WithSkipPersist(ctx), otherAuth); err != nil {
		t.Fatalf("register other: %v", err)
	}

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	runner := newAuthProviderWarmupRunner(service)
	cfg := config.AuthProviderWarmupConfig{
		Enabled:        true,
		Interval:       "1h",
		Jitter:         "0s",
		MaxConcurrency: 1,
		Prompt:         "ping",
		Providers: []config.WarmupProviderConfig{{
			Provider: "codex",
			Enabled:  true,
			Model:    "gpt-5.3-codex",
			// Intentionally NO AuthIndexes — exercise the new "warm all
			// healthy" path.
		}},
	}
	cfg.Normalize()
	runner.runOnce(ctx, cfg)

	// Drain the executor channel until idle.
	gotIDs := make(map[string]int)
	timeout := time.After(2 * time.Second)
	for collected := 0; collected < len(healthyIDs); {
		select {
		case call := <-exec.ch:
			gotIDs[call.authID]++
			if call.pinned == "" {
				t.Errorf("expected pinned warmup for auth %s, got pinned=%q", call.authID, call.pinned)
			}
			if call.synthetic != cliproxyexecutor.SyntheticRequestKindWarmup {
				t.Errorf("expected synthetic=warmup for auth %s, got %q", call.authID, call.synthetic)
			}
			collected++
		case <-timeout:
			t.Fatalf("timed out waiting for all healthy auths to be warmed; got %v", gotIDs)
		}
	}

	for _, id := range healthyIDs {
		if gotIDs[id] != 1 {
			t.Errorf("auth %s warmed %d times, want exactly 1", id, gotIDs[id])
		}
	}
	if _, leaked := gotIDs[disabledAuth.ID]; leaked {
		t.Errorf("disabled auth %s must NOT be warmed", disabledAuth.ID)
	}
	if _, leaked := gotIDs[otherAuth.ID]; leaked {
		t.Errorf("kiro auth %s must NOT be warmed by codex provider entry", otherAuth.ID)
	}
	// Also confirm no extra call snuck in after the first 3 (small grace
	// window for the runOnce goroutines to retire their work).
	time.Sleep(50 * time.Millisecond)
	for {
		select {
		case extra := <-exec.ch:
			t.Errorf("unexpected extra warmup call: %+v", extra)
		default:
			return
		}
	}
}

// TestResolveWarmupAuthsHonorsExplicitIndexes is a focused unit test on
// the resolver to make sure operator-explicit auth-indexes still pin the
// resolution to that exact list and don't accidentally pull in other
// healthy auths (regression guard against the new "warm all" code path).
func TestResolveWarmupAuthsHonorsExplicitIndexes(t *testing.T) {
	ctx := context.Background()
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)

	pinnedAuth := &coreauth.Auth{ID: "codex-pinned", Provider: "codex", Status: coreauth.StatusActive}
	otherHealthy := &coreauth.Auth{ID: "codex-other", Provider: "codex", Status: coreauth.StatusActive}
	for _, a := range []*coreauth.Auth{pinnedAuth, otherHealthy} {
		if _, err := manager.Register(coreauth.WithSkipPersist(ctx), a); err != nil {
			t.Fatalf("register %s: %v", a.ID, err)
		}
		a.EnsureIndex()
	}

	service := &Service{cfg: &config.Config{}, coreManager: manager}
	runner := newAuthProviderWarmupRunner(service)
	got := runner.resolveWarmupAuths("codex", config.WarmupProviderConfig{
		Provider:    "codex",
		Model:       "gpt-5.3-codex",
		AuthIndexes: []string{pinnedAuth.Index},
	})
	if len(got) != 1 || got[0].ID != pinnedAuth.ID {
		var ids []string
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		t.Fatalf("explicit auth-indexes path returned %v, want exactly [%s]", ids, pinnedAuth.ID)
	}
}
