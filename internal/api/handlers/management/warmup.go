package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type warmupRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	AuthID   string `json:"auth_id"`
	Prompt   string `json:"prompt"`
}

type warmupResponse struct {
	Status     string `json:"status"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	AuthID     string `json:"auth_id,omitempty"`
	AuthIndex  string `json:"auth_index,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Warmup executes a small synthetic Claude-format request through the runtime
// scheduler. Provider executors translate it to their upstream format.
// Synthetic results are ignored by auth health, quota cooling, and queue
// real-request accounting.
func (h *Handler) Warmup(c *gin.Context) {
	var req warmupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request", "message": err.Error()})
		return
	}

	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	model := strings.TrimSpace(req.Model)
	authRef := strings.TrimSpace(req.AuthID)
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = "ping"
	}

	manager := h.currentAuthManager()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth_manager_unavailable"})
		return
	}

	var pinnedAuth *coreauth.Auth
	if authRef != "" {
		if auth, ok := manager.GetByID(authRef); ok && auth != nil {
			pinnedAuth = auth
		} else if auth := h.authByIndex(authRef); auth != nil {
			pinnedAuth = auth
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth_not_found"})
			return
		}
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(pinnedAuth.Provider))
		}
	}

	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider_required"})
		return
	}
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model_required"})
		return
	}
	if !internalconfig.IsWarmupSupportedProvider(provider) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":               "provider_not_supported",
			"supported_providers": internalconfig.WarmupSupportedProviders,
		})
		return
	}

	selectedAuthID := ""
	metadata := map[string]any{
		cliproxyexecutor.SyntheticRequestMetadataKey: cliproxyexecutor.SyntheticRequestKindWarmup,
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = strings.TrimSpace(authID)
		},
	}
	if pinnedAuth != nil {
		metadata[cliproxyexecutor.PinnedAuthMetadataKey] = pinnedAuth.ID
	}

	payload, errPayload := json.Marshal(gin.H{
		"model":      model,
		"max_tokens": 1,
		"messages": []gin.H{{
			"role":    "user",
			"content": prompt,
		}},
	})
	if errPayload != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "build_request_failed"})
		return
	}

	startedAt := time.Now()
	_, errExec := manager.Execute(c.Request.Context(), []string{provider}, cliproxyexecutor.Request{
		Model:    model,
		Payload:  payload,
		Format:   sdktranslator.FormatClaude,
		Metadata: metadata,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Metadata:        metadata,
	})
	durationMS := time.Since(startedAt).Milliseconds()
	if selectedAuthID == "" && pinnedAuth != nil {
		selectedAuthID = pinnedAuth.ID
	}

	authIndex := ""
	if selectedAuthID != "" {
		if selectedAuth, ok := manager.GetByID(selectedAuthID); ok && selectedAuth != nil {
			selectedAuth.EnsureIndex()
			authIndex = selectedAuth.Index
		}
	}

	resp := warmupResponse{
		Status:     "ok",
		Provider:   provider,
		Model:      model,
		AuthID:     selectedAuthID,
		AuthIndex:  authIndex,
		DurationMS: durationMS,
	}
	if errExec != nil {
		resp.Status = "error"
		resp.Error = errExec.Error()
		c.JSON(http.StatusOK, resp)
		return
	}

	c.JSON(http.StatusOK, resp)
}

func (h *Handler) currentAuthManager() *coreauth.Manager {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.authManager
}
