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

func knownQuota(percent float64) CodexQuotaSnapshot {
	return CodexQuotaSnapshot{
		PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: percent, WindowMinutes: 300},
		Status:        CodexQuotaStatusKnown,
	}
}

func registerQueueTestAuths(t *testing.T, manager *Manager, auths ...*Auth) {
	t.Helper()
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
}

type queueLoadStore struct {
	auths []*Auth
}

func (s *queueLoadStore) List(context.Context) ([]*Auth, error) {
	out := make([]*Auth, 0, len(s.auths))
	for _, auth := range s.auths {
		if auth != nil {
			out = append(out, auth.Clone())
		}
	}
	return out, nil
}

func (s *queueLoadStore) Save(context.Context, *Auth) (string, error) { return "", nil }

func (s *queueLoadStore) Delete(context.Context, string) error { return nil }

func TestManagerLoadReconcilesCodexQueueMembershipForNewAuth(t *testing.T) {
	store := &queueLoadStore{
		auths: []*Auth{
			newQueueTestAuth("auth-1", "team-codex"),
			newQueueTestAuth("auth-2", "team-codex"),
		},
	}
	manager := NewManager(store, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.activeRefreshEvery = 0
	coordinator.standbyRefreshEvery = 0
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return knownQuota(80), nil
	}))

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)

	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("initial load: %v", err)
	}
	if got := coordinator.AuthState("auth-3"); got != nil {
		t.Fatalf("auth-3 should not exist before second load: %+v", got)
	}

	store.auths = append(store.auths, newQueueTestAuth("auth-3", "team-codex"))
	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("reload with new auth: %v", err)
	}

	state := coordinator.AuthState("auth-3")
	if state == nil {
		t.Fatalf("newly loaded codex auth missing from queue state")
	}
	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if got := len(groups[0].Members); got != 3 {
		t.Fatalf("queue member count = %d, want 3", got)
	}
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

func TestCoordinatorPromotesImmediatelyOnLowQuotaAndKeepsPreviousAvailableForPinnedRequests(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.activeRefreshEvery = 0
	coordinator.standbyRefreshEvery = 0

	now := time.Date(2026, 5, 17, 10, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }

	registerQueueTestAuths(t, manager,
		newQueueTestAuth("auth-1", "team-codex"),
		newQueueTestAuth("auth-2", "team-codex"),
	)

	cfg := mustEnabledQueueConfig()
	cfg.IdleWindow = "10m"
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)

	quotas := map[string]float64{
		"auth-1": 80,
		"auth-2": 80,
	}
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return knownQuota(quotas[auth.ID]), nil
	}))

	coordinator.Reconcile(context.Background())
	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected one global group, got %d", len(groups))
	}
	if groups[0].ActiveAuthID != "auth-1" {
		t.Fatalf("initial active = %q, want auth-1", groups[0].ActiveAuthID)
	}

	coordinator.RecordRealRequest("auth-1", now)
	quotas["auth-1"] = 2
	now = now.Add(30 * time.Second)
	coordinator.Reconcile(context.Background())

	groups = coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected one global group after low quota, got %d", len(groups))
	}
	if groups[0].ActiveAuthID != "auth-2" {
		t.Fatalf("low-quota active should promote immediately for new requests, got active %q", groups[0].ActiveAuthID)
	}
	previous := coordinator.AuthState("auth-1")
	if previous == nil {
		t.Fatalf("previous active state missing")
	}
	if previous.QueueManagedDisabled {
		t.Fatalf("previous active should stay available for pinned old requests before idle drain: %+v", previous)
	}
}

func TestCoordinatorUsesSingleGlobalGroupAcrossDifferentAuthAttributes(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()

	a1 := newQueueTestAuth("auth-1", "team-a")
	a2 := newQueueTestAuth("auth-2", "team-b")
	a2.Attributes["plan_type"] = "plus"
	a3 := newQueueTestAuth("auth-3", "team-c")
	a3.Attributes["websockets"] = "true"
	registerQueueTestAuths(t, manager, a1, a2, a3)

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return knownQuota(80), nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected one global queue group, got %d: %+v", len(groups), groups)
	}
	if groups[0].GroupKey != codexQueueGlobalGroupKey {
		t.Fatalf("group key = %q, want %q", groups[0].GroupKey, codexQueueGlobalGroupKey)
	}
	if len(groups[0].Members) != 3 {
		t.Fatalf("global group member count = %d, want 3", len(groups[0].Members))
	}
	active := groups[0].ActiveAuthID
	if active == "" {
		t.Fatalf("expected elected active auth")
	}
	for _, member := range groups[0].Members {
		blocked := coordinator.IsQueueRoutingBlocked(member.AuthID)
		if member.AuthID == active {
			if blocked {
				t.Fatalf("active auth %q must be routable", member.AuthID)
			}
			continue
		}
		if !blocked {
			t.Fatalf("standby auth %q must be routing-blocked in global queue mode", member.AuthID)
		}
	}
}

func TestCoordinatorGlobalQueueExhaustsAllAccountsWithoutFallback(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.activeRefreshEvery = 0
	coordinator.standbyRefreshEvery = 0

	now := time.Date(2026, 5, 15, 14, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }

	registerQueueTestAuths(t, manager,
		newQueueTestAuth("auth-1", "team-a"),
		newQueueTestAuth("auth-2", "team-b"),
		newQueueTestAuth("auth-3", "team-c"),
	)

	cfg := mustEnabledQueueConfig()
	cfg.IdleWindow = "1m"
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)

	quotas := map[string]float64{
		"auth-1": 80,
		"auth-2": 80,
		"auth-3": 80,
	}
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return knownQuota(quotas[auth.ID]), nil
	}))

	coordinator.Reconcile(context.Background())
	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected one global group, got %d", len(groups))
	}
	if groups[0].ActiveAuthID != "auth-1" {
		t.Fatalf("initial active = %q, want auth-1", groups[0].ActiveAuthID)
	}

	quotas["auth-1"] = 2
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	if active := coordinator.Groups()[0].ActiveAuthID; active != "auth-2" {
		t.Fatalf("after auth-1 exhaust active = %q, want auth-2", active)
	}
	if !coordinator.IsQueueManagedDisabled("auth-1") {
		t.Fatalf("auth-1 should be queue-managed disabled after switch")
	}

	quotas["auth-2"] = 2
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	if active := coordinator.Groups()[0].ActiveAuthID; active != "auth-3" {
		t.Fatalf("after auth-2 exhaust active = %q, want auth-3", active)
	}
	if !coordinator.IsQueueManagedDisabled("auth-2") {
		t.Fatalf("auth-2 should be queue-managed disabled after switch")
	}

	quotas["auth-3"] = 2
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID != "" {
		t.Fatalf("expected no active auth after all accounts exhaust, got %q", groups[0].ActiveAuthID)
	}
	if groups[0].SwitchPendingReason != CodexQueueSwitchReasonNoCandidate {
		t.Fatalf("switch reason = %q, want %q", groups[0].SwitchPendingReason, CodexQueueSwitchReasonNoCandidate)
	}
	for _, id := range []string{"auth-1", "auth-2", "auth-3"} {
		if !coordinator.IsQueueRoutingBlocked(id) {
			t.Fatalf("exhausted auth %q must stay routing-blocked when no fallback remains", id)
		}
	}

	for id := range quotas {
		quotas[id] = 80
	}
	if changed := coordinator.ResetGroup(codexQueueGlobalGroupKey); changed != 3 {
		t.Fatalf("ResetGroup changed %d auths, want 3", changed)
	}
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID != "auth-1" {
		t.Fatalf("after reset active = %q, want auth-1", groups[0].ActiveAuthID)
	}
	if coordinator.IsQueueRoutingBlocked("auth-1") {
		t.Fatalf("reset healthy active auth-1 must be routable")
	}
}

func TestCoordinatorRecordRealRequestResetsTimer(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.activeRefreshEvery = 0
	coordinator.standbyRefreshEvery = 0
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
	// fresh snapshots. New requests should move immediately to the healthy
	// standby instead of waiting for the idle window.
	coordinator.now = func() time.Time { return base.Add(6 * time.Minute) }
	coordinator.Reconcile(context.Background())
	groupsMid := coordinator.Groups()
	if groupsMid[0].ActiveAuthID == active {
		t.Fatalf("expected immediate promotion for new requests, active still %q", active)
	}
	if state := coordinator.AuthState(active); state == nil || state.QueueState != CodexQueueStateSwitchPending {
		t.Fatalf("expected previous active to stay switch_pending before idle window, got %+v", state)
	}
	if coordinator.IsQueueManagedDisabled(active) {
		t.Fatalf("previous active %q should not be queue-managed disabled before idle window", active)
	}

	// Advance well beyond the idle window since the last real request. The
	// previous active can now be queue-managed disabled while the promoted auth
	// remains active for new requests.
	coordinator.now = func() time.Time { return base.Add(15 * time.Minute) }
	coordinator.Reconcile(context.Background())
	groupsAfter := coordinator.Groups()
	if groupsAfter[0].ActiveAuthID == active {
		t.Fatalf("expected promoted auth to remain active, active still %q", active)
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

func TestCoordinatorSingleMemberGroupIsActiveAndRoutable(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	auth := newQueueTestAuth("auth-single", "team-codex")
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow:   QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			SecondaryWindow: QuotaWindowSnapshot{PercentRemaining: 40, WindowMinutes: 300},
			Status:          CodexQuotaStatusKnown,
		}, nil
	}))

	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group, got %d", len(groups))
	}
	if groups[0].ActiveAuthID != auth.ID {
		t.Fatalf("single-member group active ID = %q, want %q", groups[0].ActiveAuthID, auth.ID)
	}
	state := coordinator.AuthState(auth.ID)
	if state == nil {
		t.Fatalf("single auth state missing")
	}
	if state.QueueState != CodexQueueStateActive {
		t.Fatalf("single auth queue state = %q, want %q", state.QueueState, CodexQueueStateActive)
	}
	if coordinator.IsQueueRoutingBlocked(auth.ID) {
		t.Fatalf("single active auth must not be routing-blocked")
	}
}

func TestCoordinatorSingleManualDisabledGroupStaysManualDisabled(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	auth := newQueueTestAuth("auth-single-disabled", "team-codex")
	auth.Disabled = true
	auth.Status = StatusDisabled
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	cfg := mustEnabledQueueConfig()
	coordinator.ApplyConfig(cfg)
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("expected single group, got %d", len(groups))
	}
	if groups[0].ActiveAuthID != "" {
		t.Fatalf("manual-disabled single group active ID = %q, want empty", groups[0].ActiveAuthID)
	}
	state := coordinator.AuthState(auth.ID)
	if state == nil {
		t.Fatalf("single disabled auth state missing")
	}
	if state.QueueState != CodexQueueStateManualDisabled {
		t.Fatalf("single disabled auth queue state = %q, want %q", state.QueueState, CodexQueueStateManualDisabled)
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

func TestQueueManagedDisabledRecoversWithDwell(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.standbyRefreshEvery = 0
	coordinator.activeRefreshEvery = 0

	base := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	now := base
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
	cfg.IdleWindow = "1m"
	cfg.RecoveryDwell = "2m"
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)

	quotas := map[string]float64{"auth-1": 80, "auth-2": 80}
	coordinator.SetProvider(CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: quotas[auth.ID], WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))

	coordinator.Reconcile(context.Background())
	groups := coordinator.Groups()
	if len(groups) != 1 || groups[0].ActiveAuthID == "" {
		t.Fatalf("expected elected active, got %+v", groups)
	}
	firstActive := groups[0].ActiveAuthID
	standby := "auth-2"
	if firstActive == "auth-2" {
		standby = "auth-1"
	}

	quotas[firstActive] = 2
	quotas[standby] = 80
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID != standby {
		t.Fatalf("expected promotion to %q, got %q", standby, groups[0].ActiveAuthID)
	}
	if !coordinator.IsQueueManagedDisabled(firstActive) {
		t.Fatalf("expected %q to be queue-managed disabled", firstActive)
	}

	quotas[firstActive] = 80
	quotas[standby] = 2
	now = now.Add(2 * time.Minute)
	coordinator.Reconcile(context.Background())
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID != "" {
		t.Fatalf("no recovered candidate should be routable during dwell, active = %q", groups[0].ActiveAuthID)
	}
	if groups[0].SwitchPendingReason != CodexQueueSwitchReasonNoCandidate {
		t.Fatalf("switch reason = %q, want %q", groups[0].SwitchPendingReason, CodexQueueSwitchReasonNoCandidate)
	}
	recovered := coordinator.AuthState(firstActive)
	if recovered == nil {
		t.Fatalf("recovered auth state missing")
	}
	if recovered.QueueManagedDisabled {
		t.Fatalf("recovered auth should clear queue-managed disabled")
	}
	if recovered.QueueState != CodexQueueStateStandby {
		t.Fatalf("recovered queue_state = %q, want standby", recovered.QueueState)
	}
	if recovered.RecoveryReadyAt.IsZero() || !recovered.RecoveryReadyAt.After(now) {
		t.Fatalf("expected future recovery dwell, got %v at %v", recovered.RecoveryReadyAt, now)
	}
	if !coordinator.IsQueueManagedDisabled(standby) {
		t.Fatalf("low-quota standby should be queue-managed disabled when no recovered candidate is ready")
	}

	now = now.Add(3 * time.Minute)
	coordinator.Reconcile(context.Background())
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID != firstActive {
		t.Fatalf("expected promotion after dwell to %q, got %q", firstActive, groups[0].ActiveAuthID)
	}
}

func TestQueueManagedDisabledDoesNotRecoverFromStaleQuota(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	coordinator := manager.EnsureCodexQueueCoordinator()

	now := time.Date(2026, 5, 15, 13, 0, 0, 0, time.UTC)
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
		return CodexQuotaSnapshot{
			PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 || groups[0].ActiveAuthID == "" {
		t.Fatalf("expected elected active, got %+v", groups)
	}
	managed := "auth-2"
	if groups[0].ActiveAuthID == "auth-2" {
		managed = "auth-1"
	}

	coordinator.mu.Lock()
	state := coordinator.states[managed]
	state.QueueManagedDisabled = true
	state.QueueDisabledReason = CodexQueueDisableReasonLowQuota
	state.QueueState = CodexQueueStateManagedDisabled
	state.Quota = CodexQuotaSnapshot{
		PrimaryWindow: QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
		Status:        CodexQuotaStatusKnown,
		Stale:         true,
		FetchedAt:     now,
	}
	coordinator.mu.Unlock()

	coordinator.evaluateState(cfg)
	got := coordinator.AuthState(managed)
	if got == nil {
		t.Fatalf("managed state missing")
	}
	if !got.QueueManagedDisabled {
		t.Fatalf("stale snapshot must not clear queue-managed disabled")
	}
	if got.QueueState != CodexQueueStateManagedDisabled {
		t.Fatalf("queue_state = %q, want managed_disabled", got.QueueState)
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
