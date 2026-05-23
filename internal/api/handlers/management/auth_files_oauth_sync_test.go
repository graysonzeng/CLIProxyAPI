package management

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestSaveTokenRecordRefreshesRuntimeCodexAuthAndQueueState(t *testing.T) {
	authDir := t.TempDir()
	fileName := "codex-user@example.com-plus.json"
	ctx := context.Background()

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(ctx, &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Disabled: true,
		Status:   coreauth.StatusDisabled,
		Attributes: map[string]string{
			"path": filepath.Join(authDir, fileName),
		},
		Metadata: map[string]any{
			"type":         "codex",
			"email":        "user@example.com",
			"access_token": "old-access-token",
		},
	}); err != nil {
		t.Fatalf("register old auth: %v", err)
	}

	coordinator := manager.EnsureCodexQueueCoordinator()
	coordinator.SetProvider(coreauth.CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *coreauth.Auth) (coreauth.CodexQuotaSnapshot, error) {
		return coreauth.CodexQuotaSnapshot{
			PrimaryWindow: coreauth.QuotaWindowSnapshot{PercentRemaining: 80, WindowMinutes: 300},
			Status:        coreauth.CodexQuotaStatusKnown,
		}, nil
	}))
	queueCfg := config.CodexQueueConfig{Enabled: true}
	queueCfg.Normalize()
	coordinator.ApplyConfig(queueCfg)
	coordinator.Reconcile(ctx)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = sdkAuth.NewFileTokenStore()

	_, err := h.saveTokenRecord(ctx, &coreauth.Auth{
		ID:       fileName,
		Provider: "codex",
		FileName: fileName,
		Storage: &codex.CodexTokenStorage{
			IDToken:      "new-id-token",
			AccessToken:  "new-access-token",
			RefreshToken: "new-refresh-token",
			AccountID:    "account-1",
			Email:        "user@example.com",
			LastRefresh:  time.Now().UTC().Format(time.RFC3339),
			Expire:       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Metadata: map[string]any{
			"email":      "user@example.com",
			"account_id": "account-1",
		},
	})
	if err != nil {
		t.Fatalf("save token record: %v", err)
	}

	auth, ok := manager.GetByID(fileName)
	if !ok {
		t.Fatalf("runtime auth %q missing after token save", fileName)
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		t.Fatalf("runtime auth stayed disabled after token save: disabled=%v status=%q", auth.Disabled, auth.Status)
	}
	if got := auth.Metadata["access_token"]; got != "new-access-token" {
		t.Fatalf("runtime auth access_token = %#v, want new token", got)
	}

	state := coordinator.AuthState(fileName)
	if state == nil {
		t.Fatalf("codex queue state missing for %q", fileName)
	}
	if state.QueueState != coreauth.CodexQueueStateActive {
		t.Fatalf("codex queue state = %q, want %q", state.QueueState, coreauth.CodexQueueStateActive)
	}
}
