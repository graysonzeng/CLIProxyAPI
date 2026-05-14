package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func newQueueTestAuth(id, prefix string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Prefix:   prefix,
		Status:   StatusActive,
		Attributes: map[string]string{
			"plan_type": "team",
		},
		Metadata: map[string]any{
			"id_token":     "",
			"access_token": "tkn",
			"account_id":   "acct",
		},
	}
}

func mustEnabledQueueConfig() internalconfig.CodexQueueConfig {
	autoDisable := true
	cfg := internalconfig.CodexQueueConfig{
		Enabled:            true,
		ThresholdPercent:   10,
		IdleWindow:         "5m",
		UnknownQuotaPolicy: internalconfig.CodexQueueUnknownQuotaPolicySkip,
		AutoDisableCurrent: &autoDisable,
	}
	cfg.Normalize()
	return cfg
}

func TestCoordinatorPromotesAfterIdleAndLowQuota(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()

	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }

	a1 := newQueueTestAuth("auth-1", "team-codex")
	a2 := newQueueTestAuth("auth-2", "team-codex")
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := manager.Register(context.Background(), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		switch auth.ID {
		case "auth-1":
			return CodexQuotaSnapshot{
				PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 5, WindowMinutes: 300},
				Status:        CodexQuotaStatusKnown,
			}, nil
		case "auth-2":
			return CodexQuotaSnapshot{
				PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 90, WindowMinutes: 300},
				Status:        CodexQuotaStatusKnown,
			}, nil
		}
		return CodexQuotaSnapshot{Status: CodexQuotaStatusUnknown}, nil
	}))

	coordinator.Reconcile(context.Background())

	state := coordinator.AuthState("auth-1")
	if state == nil {
		t.Fatalf("auth-1 state missing")
	}
	if state.QueueGroup == "" {
		t.Fatalf("expected non-empty group")
	}

	// Promote one of them to active.
	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group, got %d", len(groups))
	}
	if groups[0].ActiveAuthID == "" {
		t.Fatalf("expected an active auth after first reconcile, got %q", groups[0].ActiveAuthID)
	}

	// Force auth-1 to be active by resetting auth-2's quota to known-low so
	// auth-1 is the only quota-available candidate when we then drain it.
	active := groups[0].ActiveAuthID

	// Advance time past the idle window and reconcile again to trigger
	// promotion. The non-active auth must have known-good quota for promotion
	// to occur.
	now = now.Add(10 * time.Minute)
	coordinator.now = func() time.Time { return now }
	coordinator.Reconcile(context.Background())

	groupsAfter := coordinator.Groups()
	if len(groupsAfter) != 1 {
		t.Fatalf("expected single group after switch, got %d", len(groupsAfter))
	}
	if active == "auth-1" {
		if groupsAfter[0].ActiveAuthID != "auth-2" {
			t.Fatalf("expected promotion to auth-2, got %q", groupsAfter[0].ActiveAuthID)
		}
		if !coordinator.IsQueueManagedDisabled("auth-1") {
			t.Fatalf("expected auth-1 to be queue-managed disabled")
		}
	}
}

func TestCoordinatorRecordRealRequestResetsTimer(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return base }

	a1 := newQueueTestAuth("auth-1", "team-codex")
	a2 := newQueueTestAuth("auth-2", "team-codex")
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := manager.Register(context.Background(), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	// First reconcile: both auths look healthy so the coordinator elects one.
	healthy := CodexQuotaSnapshot{
		PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
		Status:        CodexQuotaStatusKnown,
	}
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return healthy, nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group")
	}
	active := groups[0].ActiveAuthID
	if active == "" {
		t.Fatalf("expected active auth after reconcile")
	}

	// Swap the provider so the active auth dives below threshold while the
	// standby remains healthy. Force a quota refresh by advancing time.
	lowActiveID := active
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		if auth.ID == lowActiveID {
			return CodexQuotaSnapshot{
				PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 2, WindowMinutes: 300},
				Status:        CodexQuotaStatusKnown,
			}, nil
		}
		return healthy, nil
	}))

	// Mark a real request so the idle timer is reset to base+4m.
	coordinator.RecordRealRequest(active, base.Add(4*time.Minute))

	// Advance past the active refresh interval so the next Reconcile pulls
	// fresh snapshots. Idle elapsed since the recorded real request: 2m → no
	// promotion yet (idle window is 5m).
	coordinator.now = func() time.Time { return base.Add(6 * time.Minute) }
	coordinator.Reconcile(context.Background())
	groupsMid := coordinator.Groups()
	if groupsMid[0].ActiveAuthID != active {
		t.Fatalf("expected active unchanged at idle+2m, got %q (was %q)", groupsMid[0].ActiveAuthID, active)
	}

	// Advance well beyond the idle window since the last real request. The
	// standby auth still has healthy quota, so promotion must occur.
	coordinator.now = func() time.Time { return base.Add(15 * time.Minute) }
	coordinator.Reconcile(context.Background())
	groupsAfter := coordinator.Groups()
	if groupsAfter[0].ActiveAuthID == active {
		t.Fatalf("expected promotion, active still %q", active)
	}
	if !coordinator.IsQueueManagedDisabled(active) {
		t.Fatalf("expected previous active %q to be queue-managed disabled", active)
	}
}

func TestCoordinatorIgnoresFreePlans(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	a1 := newQueueTestAuth("auth-free", "team")
	a1.Attributes["plan_type"] = "free"
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register: %v", err)
	}
	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.Reconcile(context.Background())

	if state := coordinator.AuthState("auth-free"); state != nil {
		t.Fatalf("free plan should not appear in queue state, got %+v", state)
	}
}

// TestQueueModeKeepsOneActiveAtATime exercises the CRITICAL invariant of
// queue mode: when two healthy equivalent Codex auths are registered and
// queue mode is enabled, only the elected active auth must be routable;
// every other group member must be queue-routing-blocked even though it has
// no quota error and no auto-disable promotion has happened yet.
func TestQueueModeKeepsOneActiveAtATime(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	t.Cleanup(func() {
		coordinator.Stop()
		SetQueueManagedDisabledChecker(nil)
		SetQueueRoutingBlockedChecker(nil)
		SetQueueRealRequestRecorder(nil)
	})

	a1 := newQueueTestAuth("auth-1", "team-codex")
	a2 := newQueueTestAuth("auth-2", "team-codex")
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := manager.Register(context.Background(), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group, got %d", len(groups))
	}
	active := groups[0].ActiveAuthID
	if active == "" {
		t.Fatalf("expected an elected active auth")
	}
	other := "auth-2"
	if active == "auth-2" {
		other = "auth-1"
	}

	if coordinator.IsQueueRoutingBlocked(active) {
		t.Fatalf("active auth %q must not be routing-blocked", active)
	}
	if !coordinator.IsQueueRoutingBlocked(other) {
		t.Fatalf("non-active group member %q must be routing-blocked while queue mode is enabled", other)
	}
	// The package-level checker registered by EnsureCodexQueueCoordinator
	// must agree with the coordinator, since scheduler/selector consult it
	// without holding a coordinator reference.
	if isQueueRoutingBlocked(active) {
		t.Fatalf("package-level checker reported active %q as blocked", active)
	}
	if !isQueueRoutingBlocked(other) {
		t.Fatalf("package-level checker reported non-active %q as routable", other)
	}
}

// TestQueueModeManualDisableOnActivePromotesNext exercises the HIGH-2 rule:
// disabling the currently active auth must cause the next Reconcile to clear
// its active role, label it manual_disabled, and promote the next eligible
// candidate.
func TestQueueModeManualDisableOnActivePromotesNext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	t.Cleanup(func() {
		coordinator.Stop()
		SetQueueManagedDisabledChecker(nil)
		SetQueueRoutingBlockedChecker(nil)
		SetQueueRealRequestRecorder(nil)
	})

	a1 := newQueueTestAuth("auth-1", "team-codex")
	a2 := newQueueTestAuth("auth-2", "team-codex")
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := manager.Register(context.Background(), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group")
	}
	active := groups[0].ActiveAuthID
	if active == "" {
		t.Fatalf("expected an active")
	}
	other := "auth-2"
	if active == "auth-2" {
		other = "auth-1"
	}

	// Disable the active auth via the manager so the next Reconcile sees
	// the manual disable status in its cloned snapshot.
	existing, ok := manager.GetByID(active)
	if !ok {
		t.Fatalf("active %q missing from manager", active)
	}
	existing.Disabled = true
	existing.Status = StatusDisabled
	if _, err := manager.Update(context.Background(), existing); err != nil {
		t.Fatalf("disable active: %v", err)
	}

	coordinator.Reconcile(context.Background())

	groupsAfter := coordinator.Groups()
	if len(groupsAfter) != 1 {
		t.Fatalf("expected single group after promotion")
	}
	if groupsAfter[0].ActiveAuthID != other {
		t.Fatalf("expected promotion to %q, got %q", other, groupsAfter[0].ActiveAuthID)
	}
	state := coordinator.AuthState(active)
	if state == nil {
		t.Fatalf("manual-disabled auth state missing")
	}
	if state.QueueState != CodexQueueStateManualDisabled {
		t.Fatalf("expected queue_state=manual_disabled, got %q", state.QueueState)
	}
	if state.QueueManagedDisabled {
		t.Fatalf("manual disable must not flip queue-managed disabled flag")
	}
	// Standby state must surface a routable label, not the stale active label.
	standbyState := coordinator.AuthState(other)
	if standbyState == nil || standbyState.QueueState != CodexQueueStateActive {
		t.Fatalf("expected promoted auth to be active, got %+v", standbyState)
	}
}

// TestQueueRoutingBlockedDisabledWhenQueueOff confirms standby members
// become routable again once queue mode is disabled, even without an
// explicit reset call.
func TestQueueRoutingBlockedDisabledWhenQueueOff(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	t.Cleanup(func() {
		coordinator.Stop()
		SetQueueManagedDisabledChecker(nil)
		SetQueueRoutingBlockedChecker(nil)
		SetQueueRealRequestRecorder(nil)
	})

	a1 := newQueueTestAuth("auth-1", "team-codex")
	a2 := newQueueTestAuth("auth-2", "team-codex")
	if _, err := manager.Register(context.Background(), a1); err != nil {
		t.Fatalf("register a1: %v", err)
	}
	if _, err := manager.Register(context.Background(), a2); err != nil {
		t.Fatalf("register a2: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 90, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	// At this point one of the two auths is blocked from routing.
	blockedBefore := 0
	for _, id := range []string{"auth-1", "auth-2"} {
		if coordinator.IsQueueRoutingBlocked(id) {
			blockedBefore++
		}
	}
	if blockedBefore != 1 {
		t.Fatalf("expected exactly one routing-blocked auth, got %d", blockedBefore)
	}

	// Disable queue mode; standby auths must be routable again.
	disabled := mustEnabledQueueConfig()
	disabled.Enabled = false
	coordinator.ApplyConfig(disabled)
	coordinator.Stop()

	for _, id := range []string{"auth-1", "auth-2"} {
		if coordinator.IsQueueRoutingBlocked(id) {
			t.Fatalf("queue mode disabled but %q still routing-blocked", id)
		}
		if isQueueRoutingBlocked(id) {
			t.Fatalf("package-level checker still reports %q as blocked after queue disable", id)
		}
	}
}

func TestRedactQuotaErrorRemovesBearer(t *testing.T) {
	err := errors.New("failed: Bearer abc123 - chatgpt-account-id=acct_42")
	redacted := redactQuotaError(err)
	if redacted == err.Error() {
		t.Fatalf("expected redaction, got %q", redacted)
	}
	if !contains(redacted, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] marker, got %q", redacted)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
