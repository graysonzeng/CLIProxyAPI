package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// codexQueueResponse is the JSON shape returned by GetCodexQueueConfig.
type codexQueueResponse struct {
	Enabled            bool     `json:"enabled"`
	ThresholdPercent   float64  `json:"threshold-percent"`
	IdleWindow         string   `json:"idle-window"`
	RecoveryDwell      string   `json:"recovery-dwell"`
	UnknownQuotaPolicy string   `json:"unknown-quota-policy"`
	AutoDisableCurrent bool     `json:"auto-disable-current"`
	GroupBy            []string `json:"group-by"`
}

func codexQueueConfigToResponse(cfg config.CodexQueueConfig) codexQueueResponse {
	cfg.Normalize()
	return codexQueueResponse{
		Enabled:            cfg.Enabled,
		ThresholdPercent:   cfg.ThresholdPercent,
		IdleWindow:         cfg.IdleWindow,
		RecoveryDwell:      cfg.RecoveryDwell,
		UnknownQuotaPolicy: cfg.UnknownQuotaPolicy,
		AutoDisableCurrent: cfg.AutoDisableCurrentEnabled(),
		GroupBy:            cfg.EffectiveGroupBy(),
	}
}

// GetCodexQueueConfig returns the persisted routing.codex-queue configuration.
func (h *Handler) GetCodexQueueConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusOK, codexQueueResponse{
			ThresholdPercent:   config.CodexQueueDefaultThresholdPercent,
			IdleWindow:         config.CodexQueueDefaultIdleWindow,
			RecoveryDwell:      config.CodexQueueDefaultRecoveryDwell,
			UnknownQuotaPolicy: config.CodexQueueUnknownQuotaPolicySkip,
			AutoDisableCurrent: true,
			GroupBy:            (config.CodexQueueConfig{}).EffectiveGroupBy(),
		})
		return
	}
	c.JSON(http.StatusOK, codexQueueConfigToResponse(h.cfg.Routing.CodexQueue))
}

// codexQueueUpdateRequest accepts partial updates. Any nil field is left
// untouched on PATCH; PUT semantics require the full payload to be supplied
// by the client.
type codexQueueUpdateRequest struct {
	Enabled            *bool    `json:"enabled,omitempty"`
	ThresholdPercent   *float64 `json:"threshold-percent,omitempty"`
	IdleWindow         *string  `json:"idle-window,omitempty"`
	RecoveryDwell      *string  `json:"recovery-dwell,omitempty"`
	UnknownQuotaPolicy *string  `json:"unknown-quota-policy,omitempty"`
	AutoDisableCurrent *bool    `json:"auto-disable-current,omitempty"`
	GroupBy            []string `json:"group-by,omitempty"`
}

// PutCodexQueueConfig replaces (PUT) or patches (PATCH) the queue config.
func (h *Handler) PutCodexQueueConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "config_unavailable"})
		return
	}
	var body codexQueueUpdateRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body", "message": err.Error()})
		return
	}

	h.mu.Lock()
	current := h.cfg.Routing.CodexQueue
	if body.Enabled != nil {
		current.Enabled = *body.Enabled
	}
	if body.ThresholdPercent != nil {
		current.ThresholdPercent = *body.ThresholdPercent
	}
	if body.IdleWindow != nil {
		current.IdleWindow = strings.TrimSpace(*body.IdleWindow)
	}
	if body.RecoveryDwell != nil {
		current.RecoveryDwell = strings.TrimSpace(*body.RecoveryDwell)
	}
	if body.UnknownQuotaPolicy != nil {
		current.UnknownQuotaPolicy = strings.TrimSpace(*body.UnknownQuotaPolicy)
	}
	if body.AutoDisableCurrent != nil {
		v := *body.AutoDisableCurrent
		current.AutoDisableCurrent = &v
	}
	if body.GroupBy != nil {
		out := make([]string, 0, len(body.GroupBy))
		for _, item := range body.GroupBy {
			trimmed := strings.TrimSpace(item)
			if trimmed != "" {
				out = append(out, trimmed)
			}
		}
		current.GroupBy = out
	}
	if current.ThresholdPercent < 0 || current.ThresholdPercent > 100 {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid threshold-percent"})
		return
	}
	if current.IdleWindow != "" {
		if _, err := time.ParseDuration(current.IdleWindow); err != nil {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid idle-window"})
			return
		}
	}
	if current.RecoveryDwell != "" {
		if _, err := time.ParseDuration(current.RecoveryDwell); err != nil {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recovery-dwell"})
			return
		}
	}
	if current.UnknownQuotaPolicy != "" {
		policy := strings.ToLower(strings.TrimSpace(current.UnknownQuotaPolicy))
		if policy != config.CodexQueueUnknownQuotaPolicySkip && policy != config.CodexQueueUnknownQuotaPolicyPromote {
			h.mu.Unlock()
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid unknown-quota-policy"})
			return
		}
		current.UnknownQuotaPolicy = policy
	}
	current.Normalize()
	h.cfg.Routing.CodexQueue = current
	if !h.persistLocked(c) {
		h.mu.Unlock()
		return
	}
	applier := h.codexQueueApply
	h.mu.Unlock()

	// Prefer the service-level applier so the quota provider is wired
	// before the coordinator starts. Fall back to the auth manager when
	// the service has not registered a callback (e.g. tests).
	if applier != nil {
		applier(current)
		return
	}
	if h.authManager != nil {
		h.authManager.ApplyCodexQueueConfig(c.Request.Context(), current)
	}
}

// GetCodexQueueState returns the runtime queue state snapshot.
func (h *Handler) GetCodexQueueState(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "groups": []any{}})
		return
	}
	coordinator := h.authManager.CodexQueueCoordinator()
	if coordinator == nil || !coordinator.Enabled() {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "groups": []any{}})
		return
	}
	cfg := coordinator.Config()
	c.JSON(http.StatusOK, gin.H{
		"enabled": true,
		"config":  codexQueueConfigToResponse(cfg),
		"groups":  coordinator.Groups(),
	})
}

// codexQueueResetRequest selects a reset target.
type codexQueueResetRequest struct {
	AuthID   string `json:"auth_id,omitempty"`
	GroupKey string `json:"group_key,omitempty"`
}

// ResetCodexQueueAuth clears queue-managed disabled state for an auth or group.
// The endpoint is intentionally additive — manual disabled state remains intact.
func (h *Handler) ResetCodexQueueAuth(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth_manager_unavailable"})
		return
	}
	coordinator := h.authManager.CodexQueueCoordinator()
	if coordinator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "queue_not_enabled"})
		return
	}
	var body codexQueueResetRequest
	_ = c.ShouldBindJSON(&body)
	authID := strings.TrimSpace(body.AuthID)
	groupKey := strings.TrimSpace(body.GroupKey)
	if authID == "" && groupKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id_or_group_key_required"})
		return
	}
	if authID != "" {
		if !coordinator.ResetAuth(authID) {
			c.JSON(http.StatusOK, gin.H{"ok": false, "changed": 0})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "changed": 1})
		return
	}
	count := coordinator.ResetGroup(groupKey)
	c.JSON(http.StatusOK, gin.H{"ok": count > 0, "changed": count})
}

// Compile-time assertion to keep coreauth import even when the queue methods
// above don't directly reference exported symbols beyond the manager.
var _ = (*coreauth.Manager)(nil)
