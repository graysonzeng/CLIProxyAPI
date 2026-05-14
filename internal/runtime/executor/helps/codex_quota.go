package helps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// CodexQuotaProbeURL is the upstream endpoint used to fetch Codex rate-limit
// snapshots. Exported so tests and operators can override it via build tags
// or future config knobs if necessary.
const CodexQuotaProbeURL = "https://chatgpt.com/backend-api/wham/usage"

// CodexUsageWindow mirrors one entry inside `rate_limit.*_window` returned by
// the wham/usage endpoint.
type CodexUsageWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetAt       int64   `json:"reset_at"`
}

// CodexUsageRateLimit mirrors the `rate_limit` object inside the wham/usage
// response payload.
type CodexUsageRateLimit struct {
	PrimaryWindow   CodexUsageWindow `json:"primary_window"`
	SecondaryWindow CodexUsageWindow `json:"secondary_window"`
}

// CodexUsageResponse is the minimal subset of the wham/usage payload the
// coordinator needs. Unknown fields are ignored.
type CodexUsageResponse struct {
	RateLimit CodexUsageRateLimit `json:"rate_limit"`
	PlanType  string              `json:"plan_type"`
}

// NewCodexQueueQuotaProvider returns a provider that fetches Codex usage data
// from chatgpt.com/backend-api/wham/usage using the same proxy-aware HTTP
// client as the main executor path. The provider must be wired into the
// auth.Manager via SetCodexQueueQuotaProvider.
//
// The provider intentionally returns an empty snapshot with Status set to
// "unknown" when the auth lacks credentials, so the coordinator can decide
// whether to skip or promote based on the unknown-quota policy.
func NewCodexQueueQuotaProvider(cfgGetter func() *config.Config) cliproxyauth.CodexQueueQuotaProvider {
	return cliproxyauth.CodexQueueQuotaProviderFunc(func(ctx context.Context, auth *cliproxyauth.Auth) (cliproxyauth.CodexQuotaSnapshot, error) {
		snapshot := cliproxyauth.CodexQuotaSnapshot{Source: "wham/usage"}
		if auth == nil {
			return snapshot, fmt.Errorf("codex quota probe: auth is nil")
		}
		token := ""
		accountID := ""
		if auth.Metadata != nil {
			if v, ok := auth.Metadata["access_token"].(string); ok {
				token = strings.TrimSpace(v)
			}
			if v, ok := auth.Metadata["account_id"].(string); ok {
				accountID = strings.TrimSpace(v)
			}
		}
		if token == "" {
			snapshot.Status = cliproxyauth.CodexQuotaStatusUnknown
			return snapshot, nil
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, CodexQuotaProbeURL, nil)
		if err != nil {
			return snapshot, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)
		if accountID != "" {
			httpReq.Header.Set("Chatgpt-Account-Id", accountID)
		}
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("User-Agent", "cli-proxy-api/codex-queue")

		cfg := (*config.Config)(nil)
		if cfgGetter != nil {
			cfg = cfgGetter()
		}
		client := NewProxyAwareHTTPClient(ctx, cfg, auth, 0)

		resp, err := client.Do(httpReq)
		if err != nil {
			return snapshot, err
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				snapshot.Error = "codex quota probe: " + errClose.Error()
			}
		}()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return snapshot, err
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// Auth errors are transient: the existing auth manager refresh
			// loop will renew the OAuth token on its own cadence. Mark the
			// snapshot quota_unknown so the coordinator does not classify a
			// refreshable token as a permanent error, and so the default
			// unknown-quota-policy=skip keeps the auth out of promotion
			// until the next probe succeeds.
			snapshot.Status = cliproxyauth.CodexQuotaStatusUnknown
			snapshot.Error = fmt.Sprintf("codex quota probe: http %d (token refresh pending)", resp.StatusCode)
			return snapshot, nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return snapshot, fmt.Errorf("codex quota probe: http %d", resp.StatusCode)
		}

		var parsed CodexUsageResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			return snapshot, fmt.Errorf("codex quota probe: decode failed: %w", err)
		}

		snapshot.PrimaryWindow = convertCodexUsageWindow(parsed.RateLimit.PrimaryWindow)
		snapshot.SecondaryWindow = convertCodexUsageWindow(parsed.RateLimit.SecondaryWindow)
		snapshot.Status = cliproxyauth.CodexQuotaStatusKnown
		return snapshot, nil
	})
}

func convertCodexUsageWindow(w CodexUsageWindow) cliproxyauth.QuotaWindowSnapshot {
	if w.WindowMinutes == 0 && w.ResetAt == 0 && w.UsedPercent == 0 {
		return cliproxyauth.QuotaWindowSnapshot{}
	}
	remaining := 100 - w.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	out := cliproxyauth.QuotaWindowSnapshot{
		PercentRemaining: remaining,
		WindowMinutes:    w.WindowMinutes,
	}
	if w.ResetAt > 0 {
		out.ResetAt = time.Unix(w.ResetAt, 0).UTC()
	}
	return out
}
