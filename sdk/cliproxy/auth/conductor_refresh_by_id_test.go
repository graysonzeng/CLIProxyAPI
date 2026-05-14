package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type refreshByIDExecutor struct {
	calls int
}

type blockingRefreshEvaluator struct {
	entered chan struct{}
	release chan struct{}
}

func (e *blockingRefreshEvaluator) ShouldRefresh(time.Time, *Auth) bool {
	close(e.entered)
	<-e.release
	return true
}

func (e *refreshByIDExecutor) Identifier() string { return "kiro" }

func (e *refreshByIDExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshByIDExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *refreshByIDExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	updated := auth.Clone()
	if updated.Metadata == nil {
		updated.Metadata = make(map[string]any)
	}
	updated.Metadata["accessToken"] = "refreshed"
	updated.Metadata["expiresAt"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return updated, nil
}

func (e *refreshByIDExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *refreshByIDExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerRefreshAuthByIDRefreshesDueAuth(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	exec := &refreshByIDExecutor{}
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:       "kiro-auth",
		Provider: "kiro",
		Metadata: map[string]any{
			"expiresAt":        time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
			"refresh_interval": "10m",
		},
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	updated, attempted, err := mgr.RefreshAuthByID(ctx, auth.ID)
	if err != nil {
		t.Fatalf("RefreshAuthByID returned error: %v", err)
	}
	if !attempted {
		t.Fatal("RefreshAuthByID attempted = false, want true")
	}
	if exec.calls != 1 {
		t.Fatalf("Refresh calls = %d, want 1", exec.calls)
	}
	if updated == nil || updated.Metadata["accessToken"] != "refreshed" {
		t.Fatalf("updated auth missing refreshed token: %#v", updated)
	}
}

func TestManagerRefreshAuthByIDSkipsFreshAuth(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	exec := &refreshByIDExecutor{}
	mgr.RegisterExecutor(exec)
	auth := &Auth{
		ID:              "kiro-auth",
		Provider:        "kiro",
		LastRefreshedAt: time.Now(),
		Metadata: map[string]any{
			"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	updated, attempted, err := mgr.RefreshAuthByID(ctx, auth.ID)
	if err != nil {
		t.Fatalf("RefreshAuthByID returned error: %v", err)
	}
	if attempted {
		t.Fatal("RefreshAuthByID attempted = true, want false")
	}
	if exec.calls != 0 {
		t.Fatalf("Refresh calls = %d, want 0", exec.calls)
	}
	if updated == nil || updated.ID != auth.ID {
		t.Fatalf("updated auth = %#v, want %s", updated, auth.ID)
	}
}

func TestManagerRefreshAuthByIDSuppressesPendingRefresh(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(nil, nil, nil)
	exec := &refreshByIDExecutor{}
	mgr.RegisterExecutor(exec)
	evaluator := &blockingRefreshEvaluator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	auth := &Auth{
		ID:       "kiro-auth",
		Provider: "kiro",
		Runtime:  evaluator,
	}
	if _, err := mgr.Register(WithSkipPersist(ctx), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	type refreshResult struct {
		auth      *Auth
		attempted bool
		err       error
	}
	resultCh := make(chan refreshResult, 1)
	go func() {
		updated, attempted, err := mgr.RefreshAuthByID(ctx, auth.ID)
		resultCh <- refreshResult{auth: updated, attempted: attempted, err: err}
	}()

	select {
	case <-evaluator.entered:
	case <-time.After(time.Second):
		t.Fatal("RefreshAuthByID did not reach refresh evaluator")
	}
	if !mgr.markRefreshPending(auth.ID, time.Now()) {
		t.Fatal("markRefreshPending returned false before test-owned pending refresh")
	}
	close(evaluator.release)

	var result refreshResult
	select {
	case result = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("RefreshAuthByID did not return")
	}
	if result.err != nil {
		t.Fatalf("RefreshAuthByID returned error: %v", result.err)
	}
	if result.attempted {
		t.Fatal("RefreshAuthByID attempted = true, want false")
	}
	if exec.calls != 0 {
		t.Fatalf("Refresh calls = %d, want 0", exec.calls)
	}
	if result.auth == nil || result.auth.ID != auth.ID {
		t.Fatalf("updated auth = %#v, want %s", result.auth, auth.ID)
	}
	if result.auth.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter is zero, want pending refresh snapshot")
	}
}
