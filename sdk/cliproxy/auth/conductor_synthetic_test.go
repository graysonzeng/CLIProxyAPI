package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type syntheticStatusError struct {
	status int
	msg    string
}

func (e syntheticStatusError) Error() string {
	return e.msg
}

func (e syntheticStatusError) StatusCode() int {
	return e.status
}

type syntheticFailureExecutor struct{}

func (syntheticFailureExecutor) Identifier() string { return "synthetic" }

func (syntheticFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, syntheticStatusError{status: http.StatusTooManyRequests, msg: "quota exhausted"}
}

func (syntheticFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, syntheticStatusError{status: http.StatusTooManyRequests, msg: "quota exhausted"}
}

func (syntheticFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, nil
}

func (syntheticFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, syntheticStatusError{status: http.StatusTooManyRequests, msg: "quota exhausted"}
}

func (syntheticFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerMarkResultSyntheticFailureDoesNotAffectAuthHealth(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "synthetic",
		Metadata: map[string]any{
			"type": "synthetic",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{
		AuthID:    auth.ID,
		Provider:  auth.Provider,
		Model:     "warmup-model",
		Success:   false,
		Synthetic: true,
		Error:     &Error{Message: "quota exhausted", HTTPStatus: http.StatusTooManyRequests},
	})

	gotAuth, ok := mgr.GetByID(auth.ID)
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}
	if gotAuth.Success != 0 || gotAuth.Failed != 0 {
		t.Fatalf("synthetic result changed totals = %d/%d, want 0/0", gotAuth.Success, gotAuth.Failed)
	}
	if gotAuth.Unavailable {
		t.Fatal("synthetic failure marked auth unavailable")
	}
	if gotAuth.Quota.Exceeded {
		t.Fatal("synthetic failure marked auth quota exceeded")
	}
	if state := gotAuth.ModelStates["warmup-model"]; state != nil {
		t.Fatalf("synthetic failure created model state: %#v", state)
	}
	snapshot := gotAuth.RecentRequestsSnapshot(time.Now())
	for _, bucket := range snapshot {
		if bucket.Success != 0 || bucket.Failed != 0 {
			t.Fatalf("synthetic failure changed recent request bucket %#v", bucket)
		}
	}
}

type syntheticSuccessExecutor struct {
	provider     string
	authID       string
	streamAuthID string
}

func (e *syntheticSuccessExecutor) Identifier() string { return e.provider }

func (e *syntheticSuccessExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.authID = auth.ID
	return cliproxyexecutor.Response{Payload: []byte(`{}`)}, nil
}

func (e *syntheticSuccessExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.streamAuthID = auth.ID
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *syntheticSuccessExecutor) Refresh(context.Context, *Auth) (*Auth, error) {
	return nil, nil
}

func (e *syntheticSuccessExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *syntheticSuccessExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerExecuteSyntheticWarmupCanPinQueueBlockedCodexAuth(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &syntheticSuccessExecutor{provider: "codex"}
	mgr.RegisterExecutor(exec)

	a1 := newQueueTestAuth("codex-auth-1", "team-codex")
	a2 := newQueueTestAuth("codex-auth-2", "team-codex")
	if _, err := mgr.Register(WithSkipPersist(ctx), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(a1.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a1.ID)
	registry.GetGlobalRegistry().RegisterClient(a2.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a2.ID)

	coordinator := mgr.EnsureCodexQueueCoordinator()
	cfg := internalconfig.CodexQueueConfig{Enabled: true, ThresholdPercent: 10, IdleWindow: "1m"}
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(ctx)

	groups := coordinator.Groups()
	if len(groups) != 1 || groups[0].ActiveAuthID == "" {
		t.Fatalf("expected elected active, got %+v", groups)
	}
	pinned := "codex-auth-2"
	if groups[0].ActiveAuthID == pinned {
		pinned = "codex-auth-1"
	}

	coordinator.mu.Lock()
	state := coordinator.states[pinned]
	if state == nil {
		coordinator.mu.Unlock()
		t.Fatalf("pinned auth state missing")
	}
	state.QueueManagedDisabled = true
	state.QueueDisabledReason = CodexQueueDisableReasonLowQuota
	state.QueueState = CodexQueueStateManagedDisabled
	beforeLastRealRequestAt := state.LastRealRequestAt
	beforeQueueState := state.QueueState
	coordinator.mu.Unlock()
	coordinator.pushSchedulerUpdate(pinned)
	if !coordinator.IsQueueRoutingBlocked(pinned) {
		t.Fatalf("expected pinned auth to be queue-routing blocked")
	}

	_, err := mgr.Execute(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.3-codex"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SyntheticRequestMetadataKey: cliproxyexecutor.SyntheticRequestKindWarmup,
			cliproxyexecutor.PinnedAuthMetadataKey:       pinned,
		},
	})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if exec.authID != pinned {
		t.Fatalf("executor auth ID = %q, want %q", exec.authID, pinned)
	}
	after := coordinator.AuthState(pinned)
	if after == nil {
		t.Fatalf("pinned auth state missing after execute")
	}
	if after.LastRealRequestAt != beforeLastRealRequestAt {
		t.Fatalf("synthetic warmup changed LastRealRequestAt: before=%v after=%v", beforeLastRealRequestAt, after.LastRealRequestAt)
	}
	if after.QueueState != beforeQueueState || !after.QueueManagedDisabled {
		t.Fatalf("synthetic warmup changed queue state: %+v", after)
	}
}

func TestManagerExecuteStreamSyntheticWarmupCanPinQueueBlockedCodexAuth(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &syntheticSuccessExecutor{provider: "codex"}
	mgr.RegisterExecutor(exec)

	a1 := newQueueTestAuth("codex-stream-auth-1", "team-codex-stream")
	a2 := newQueueTestAuth("codex-stream-auth-2", "team-codex-stream")
	if _, err := mgr.Register(WithSkipPersist(ctx), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(a1.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a1.ID)
	registry.GetGlobalRegistry().RegisterClient(a2.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.3-codex", Type: "openai"}})
	defer registry.GetGlobalRegistry().UnregisterClient(a2.ID)

	coordinator := mgr.EnsureCodexQueueCoordinator()
	cfg := internalconfig.CodexQueueConfig{Enabled: true, ThresholdPercent: 10, IdleWindow: "1m"}
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(ctx)

	groups := coordinator.Groups()
	if len(groups) != 1 || groups[0].ActiveAuthID == "" {
		t.Fatalf("expected elected active, got %+v", groups)
	}
	pinned := "codex-stream-auth-2"
	if groups[0].ActiveAuthID == pinned {
		pinned = "codex-stream-auth-1"
	}

	coordinator.mu.Lock()
	state := coordinator.states[pinned]
	if state == nil {
		coordinator.mu.Unlock()
		t.Fatalf("pinned auth state missing")
	}
	state.QueueManagedDisabled = true
	state.QueueDisabledReason = CodexQueueDisableReasonLowQuota
	state.QueueState = CodexQueueStateManagedDisabled
	beforeLastRealRequestAt := state.LastRealRequestAt
	beforeQueueState := state.QueueState
	coordinator.mu.Unlock()
	coordinator.pushSchedulerUpdate(pinned)
	if !coordinator.IsQueueRoutingBlocked(pinned) {
		t.Fatalf("expected pinned auth to be queue-routing blocked")
	}

	streamResult, err := mgr.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5.3-codex"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SyntheticRequestMetadataKey: cliproxyexecutor.SyntheticRequestKindWarmup,
			cliproxyexecutor.PinnedAuthMetadataKey:       pinned,
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream returned error: %v", err)
	}
	for range streamResult.Chunks {
	}
	if exec.streamAuthID != pinned {
		t.Fatalf("stream executor auth ID = %q, want %q", exec.streamAuthID, pinned)
	}
	after := coordinator.AuthState(pinned)
	if after == nil {
		t.Fatalf("pinned auth state missing after execute stream")
	}
	if after.LastRealRequestAt != beforeLastRealRequestAt {
		t.Fatalf("synthetic stream warmup changed LastRealRequestAt: before=%v after=%v", beforeLastRealRequestAt, after.LastRealRequestAt)
	}
	if after.QueueState != beforeQueueState || !after.QueueManagedDisabled {
		t.Fatalf("synthetic stream warmup changed queue state: %+v", after)
	}
}

func TestManagerExecuteSyntheticFailureDoesNotCooldownAuth(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, &RoundRobinSelector{}, nil)
	mgr.RegisterExecutor(syntheticFailureExecutor{})
	auth := &Auth{
		ID:       "auth-1",
		Provider: "synthetic",
		Metadata: map[string]any{
			"type": "synthetic",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	registerSchedulerModels(t, "synthetic", "warmup-model", auth.ID)

	_, err := mgr.Execute(ctx, []string{"synthetic"}, cliproxyexecutor.Request{
		Model: "warmup-model",
	}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.SyntheticRequestMetadataKey: cliproxyexecutor.SyntheticRequestKindWarmup,
			cliproxyexecutor.PinnedAuthMetadataKey:       auth.ID,
		},
	})
	if err == nil {
		t.Fatal("Execute returned nil error, want synthetic upstream failure")
	}

	gotAuth, ok := mgr.GetByID(auth.ID)
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}
	if gotAuth.Failed != 0 {
		t.Fatalf("synthetic Execute changed failed total = %d, want 0", gotAuth.Failed)
	}
	if gotAuth.Unavailable || gotAuth.Quota.Exceeded || !gotAuth.NextRetryAfter.IsZero() {
		t.Fatalf("synthetic Execute changed auth health: unavailable=%v quota=%v next=%v", gotAuth.Unavailable, gotAuth.Quota, gotAuth.NextRetryAfter)
	}
	if state := gotAuth.ModelStates["warmup-model"]; state != nil {
		t.Fatalf("synthetic Execute created model state: %#v", state)
	}
}
