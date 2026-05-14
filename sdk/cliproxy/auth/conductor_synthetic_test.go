package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

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
