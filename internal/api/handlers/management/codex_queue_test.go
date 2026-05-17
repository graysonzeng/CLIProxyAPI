package management

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newManagementCodexQueueTestAuth(id string) *coreauth.Auth {
	return &coreauth.Auth{
		ID:       id,
		Provider: "codex",
		Prefix:   "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"plan_type": "team",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"account_id":   "account",
		},
	}
}

func TestResetCodexQueueGroupReconcilesActiveImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), newManagementCodexQueueTestAuth("auth-1")); err != nil {
		t.Fatalf("register auth-1: %v", err)
	}
	if _, err := manager.Register(context.Background(), newManagementCodexQueueTestAuth("auth-2")); err != nil {
		t.Fatalf("register auth-2: %v", err)
	}

	coordinator := manager.EnsureCodexQueueCoordinator()
	cfg := internalconfig.CodexQueueConfig{
		Enabled:            true,
		ThresholdPercent:   10,
		IdleWindow:         "1m",
		UnknownQuotaPolicy: internalconfig.CodexQueueUnknownQuotaPolicySkip,
	}
	cfg.Normalize()
	coordinator.ApplyConfig(cfg)
	coordinator.SetProvider(coreauth.CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *coreauth.Auth) (coreauth.CodexQuotaSnapshot, error) {
		return coreauth.CodexQuotaSnapshot{
			PrimaryWindow: coreauth.QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        coreauth.CodexQuotaStatusKnown,
		}, nil
	}))
	coordinator.Reconcile(context.Background())

	groups := coordinator.Groups()
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if groups[0].ActiveAuthID == "" {
		t.Fatalf("expected active auth before reset")
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v0/management/routing/codex-queue/reset",
		bytes.NewBufferString(`{"group_key":"`+groups[0].GroupKey+`"}`),
	)

	h.ResetCodexQueueAuth(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	groups = coordinator.Groups()
	if groups[0].ActiveAuthID == "" {
		t.Fatalf("expected active auth immediately after group reset")
	}
}
