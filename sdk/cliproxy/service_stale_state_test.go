package cliproxy

import (
	"context"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestServiceApplyCoreAuthAddOrUpdate_DeleteReAddDoesNotInheritStaleRuntimeState(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "service-stale-state-auth"
	modelID := "stale-model"
	lastRefreshedAt := time.Date(2026, time.March, 1, 8, 0, 0, 0, time.UTC)
	nextRefreshAfter := lastRefreshedAt.Add(30 * time.Minute)

	t.Cleanup(func() {
		GlobalModelRegistry().UnregisterClient(authID)
	})

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:               authID,
		Provider:         "claude",
		Status:           coreauth.StatusActive,
		LastRefreshedAt:  lastRefreshedAt,
		NextRefreshAfter: nextRefreshAfter,
		ModelStates: map[string]*coreauth.ModelState{
			modelID: {
				Quota: coreauth.QuotaState{BackoffLevel: 7},
			},
		},
	})

	service.applyCoreAuthRemoval(context.Background(), authID)

	if removed, ok := service.coreManager.GetByID(authID); ok || removed != nil {
		t.Fatalf("expected removed auth to be absent, got %+v", removed)
	}

	service.applyCoreAuthAddOrUpdate(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
	})

	updated, ok := service.coreManager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("expected re-added auth to be present")
	}
	if updated.Disabled {
		t.Fatalf("expected re-added auth to be active")
	}
	if !updated.LastRefreshedAt.IsZero() {
		t.Fatalf("expected LastRefreshedAt to reset on delete -> re-add, got %v", updated.LastRefreshedAt)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected NextRefreshAfter to reset on delete -> re-add, got %v", updated.NextRefreshAfter)
	}
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to reset on delete -> re-add, got %d entries", len(updated.ModelStates))
	}
	if models := registry.GetGlobalRegistry().GetModelsForClient(authID); len(models) == 0 {
		t.Fatalf("expected re-added auth to re-register models in global registry")
	}
}

func TestServiceApplyCoreAuthRemovalPrunesCodexQueueMember(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{
		cfg:         &config.Config{},
		coreManager: manager,
	}
	coordinator := manager.EnsureCodexQueueCoordinator()
	t.Cleanup(func() {
		coordinator.Stop()
		coreauth.SetQueueManagedDisabledChecker(nil)
		coreauth.SetQueueRoutingBlockedChecker(nil)
		coreauth.SetQueueRealRequestRecorder(nil)
	})

	for _, id := range []string{"codex-auth-1", "codex-auth-2"} {
		if _, err := manager.Register(context.Background(), &coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"plan_type": "plus"},
		}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	queueCfg := config.CodexQueueConfig{
		Enabled:          true,
		ThresholdPercent: 10,
		IdleWindow:       "1m",
	}
	queueCfg.Normalize()
	coordinator.ApplyConfig(queueCfg)
	coordinator.SetProvider(coreauth.CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *coreauth.Auth) (coreauth.CodexQuotaSnapshot, error) {
		return coreauth.CodexQuotaSnapshot{
			PrimaryWindow: coreauth.QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        coreauth.CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	service.applyCoreAuthRemoval(context.Background(), "codex-auth-1")

	if removed, ok := manager.GetByID("codex-auth-1"); ok || removed != nil {
		t.Fatalf("removed auth still present in manager: %+v", removed)
	}
	if state := coordinator.AuthState("codex-auth-1"); state != nil {
		t.Fatalf("removed auth still present in queue state: %+v", state)
	}
	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("queue groups = %d, want 1", len(groups))
	}
	if got := len(groups[0].Members); got != 1 {
		t.Fatalf("queue member count after removal = %d, want 1", got)
	}
	if groups[0].Members[0].AuthID != "codex-auth-2" {
		t.Fatalf("remaining queue member = %q, want codex-auth-2", groups[0].Members[0].AuthID)
	}
}

func TestForceHomeRuntimeConfigEnablesUsageStatistics(t *testing.T) {
	cfg := &config.Config{
		UsageStatisticsEnabled: false,
	}

	forceHomeRuntimeConfig(cfg)

	if !cfg.UsageStatisticsEnabled {
		t.Fatal("expected home runtime config to force usage statistics enabled")
	}
}

func TestApplyHomeOverlayForcesUsageStatisticsEnabled(t *testing.T) {
	baseCfg := &config.Config{}
	baseCfg.Home.Enabled = true
	service := &Service{cfg: baseCfg}

	service.applyHomeOverlay(&config.Config{
		UsageStatisticsEnabled: false,
	})

	if service.cfg == nil || !service.cfg.UsageStatisticsEnabled {
		t.Fatal("expected home overlay to force usage statistics enabled")
	}
	if !service.cfg.Home.Enabled {
		t.Fatal("expected home overlay to preserve local home settings")
	}
}
