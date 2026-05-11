package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// regionRegex validates AWS region format (e.g. us-east-1, eu-west-2).
var regionRegex = regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]+$`)

// ImportKiroToken handles importing a Kiro credential JSON via paste.
// It validates the token by calling the Kiro refresh API before saving.
func (h *Handler) ImportKiroToken(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	if h.cfg.AuthDir == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth directory not configured"})
		return
	}

	// Limit request body to 64KB — Kiro credential JSON is small.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)

	var body map[string]any
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}

	refreshToken := stringFromMap(body, "refreshToken")
	if refreshToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refreshToken is required"})
		return
	}

	authMethod := stringFromMap(body, "authMethod")
	clientID := stringFromMap(body, "clientId")
	clientSecret := stringFromMap(body, "clientSecret")
	hasIDCCreds := clientID != "" && clientSecret != ""

	if authMethod == "" {
		if hasIDCCreds {
			authMethod = "builder-id"
		} else {
			authMethod = helps.KiroAuthMethodSocial
		}
	}
	if authMethod != helps.KiroAuthMethodSocial && authMethod != "builder-id" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "authMethod must be 'social' or 'builder-id'"})
		return
	}

	if authMethod == "builder-id" {
		if clientID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "clientId is required for builder-id auth"})
			return
		}
		if clientSecret == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "clientSecret is required for builder-id auth"})
			return
		}
	}

	region := stringFromMap(body, "region")
	if region == "" {
		region = helps.KiroDefaultRegion
	}
	idcRegion := stringFromMap(body, "idcRegion")
	if idcRegion == "" {
		idcRegion = helps.KiroIDCRegion
	}

	if !regionRegex.MatchString(region) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid region format"})
		return
	}
	if !regionRegex.MatchString(idcRegion) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid idcRegion format"})
		return
	}

	// Build refresh request
	reqBody := map[string]string{"refreshToken": refreshToken}
	var refreshURL string
	isSocial := authMethod == helps.KiroAuthMethodSocial
	if isSocial {
		refreshURL = helps.KiroSocialRefreshURL(region)
	} else {
		refreshURL = helps.KiroIDCRefreshURL(idcRegion)
		reqBody["clientId"] = clientID
		reqBody["clientSecret"] = clientSecret
		reqBody["grantType"] = "refresh_token"
	}

	bodyBytes, _ := json.Marshal(reqBody)
	ctx := c.Request.Context()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(bodyBytes))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("kiro token refresh failed: %v", err)})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := helps.NewProxyAwareHTTPClient(ctx, h.cfg, nil, 15*time.Second)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro token refresh failed: network error"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro token refresh failed: read error"})
		return
	}

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro refresh token expired or invalid, obtain a new one from Kiro IDE"})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("kiro token refresh failed: status %d", resp.StatusCode)})
		return
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro refresh response missing accessToken"})
		return
	}
	if result.AccessToken == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "kiro refresh response missing accessToken"})
		return
	}

	// Build metadata: start with input fields, then overlay refresh response
	now := time.Now()
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	metadata := map[string]any{
		"type":         "kiro",
		"refreshToken": refreshToken,
		"accessToken":  result.AccessToken,
		"authMethod":   authMethod,
		"region":       region,
		"expiresAt":    now.Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339),
		"last_refresh": now.Format(time.RFC3339),
		"timestamp":    now.UnixMilli(),
	}
	// Overlay refresh response fields if non-empty
	if result.RefreshToken != "" {
		metadata["refreshToken"] = result.RefreshToken
	}
	if result.ProfileArn != "" {
		metadata["profileArn"] = result.ProfileArn
	}
	// Preserve optional input fields
	if v := stringFromMap(body, "profileArn"); v != "" && result.ProfileArn == "" {
		metadata["profileArn"] = v
	}
	if v := stringFromMap(body, "uuid"); v != "" {
		metadata["uuid"] = v
	}
	if authMethod == "builder-id" {
		metadata["clientId"] = clientID
		metadata["clientSecret"] = clientSecret
		metadata["idcRegion"] = idcRegion
	}

	fileName := fmt.Sprintf("kiro-%d.json", now.UnixMilli())
	label := "Kiro (social)"
	if authMethod == "builder-id" {
		label = "Kiro (builder-id)"
	}

	record := &coreauth.Auth{
		ID:       fileName,
		Provider: "kiro",
		FileName: fileName,
		Label:    label,
		Metadata: metadata,
	}

	savedPath, err := h.saveTokenRecord(c.Request.Context(), record)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save auth record: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"file":    savedPath,
		"message": "Kiro token validated and saved",
	})
}

func stringFromMap(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}
