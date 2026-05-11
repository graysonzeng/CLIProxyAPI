package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func setupKiroTestHandler(t *testing.T) *Handler {
	t.Helper()
	return &Handler{
		cfg:            &config.Config{AuthDir: t.TempDir()},
		tokenStore:     &memoryAuthStore{},
		failedAttempts: make(map[string]*attemptInfo),
	}
}

func doKiroImport(h *Handler, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/kiro-token", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportKiroToken(c)
	return w
}

func TestImportKiroToken_MissingRefreshToken(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"authMethod":"social"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestImportKiroToken_InvalidAuthMethod(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"invalid"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestImportKiroToken_BuilderIdMissingClientId(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"builder-id","clientSecret":"s"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestImportKiroToken_BuilderIdMissingClientSecret(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"builder-id","clientId":"c"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestImportKiroToken_MalformedJSON(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestImportKiroToken_AuthMethodInference(t *testing.T) {
	// With clientId+clientSecret but no authMethod → should infer builder-id
	// This will fail at refresh (502) but validates inference path
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","clientId":"c","clientSecret":"s"}`)
	// Should get past validation (not 400) → will fail at refresh call (502)
	if w.Code == http.StatusBadRequest {
		t.Fatalf("expected non-400 (inference should pass validation), got 400")
	}
}

func TestImportKiroToken_SocialSuccess(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"accessToken":  "at-123",
			"refreshToken": "rt-new",
			"profileArn":   "arn:aws:iam::123:profile/test",
			"expiresIn":    3600,
		})
	}))
	defer mockServer.Close()

	// Patch URL template to point to mock server
	orig := helps.KiroSocialRefreshURLTemplate
	helps.KiroSocialRefreshURLTemplate = mockServer.URL + "/refreshToken"
	defer func() { helps.KiroSocialRefreshURLTemplate = orig }()

	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"social"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Verify saved record
	store := h.tokenStore.(*memoryAuthStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.items) == 0 {
		t.Fatal("expected auth record to be saved")
	}
	for _, rec := range store.items {
		if rec.Metadata["accessToken"] != "at-123" {
			t.Errorf("expected accessToken at-123, got %v", rec.Metadata["accessToken"])
		}
		if rec.Metadata["refreshToken"] != "rt-new" {
			t.Errorf("expected refreshToken rt-new, got %v", rec.Metadata["refreshToken"])
		}
		if rec.Metadata["profileArn"] != "arn:aws:iam::123:profile/test" {
			t.Errorf("expected profileArn from response, got %v", rec.Metadata["profileArn"])
		}
		break
	}
}

func TestImportKiroToken_ConfigUnavailable(t *testing.T) {
	h := &Handler{
		cfg:            nil,
		failedAttempts: make(map[string]*attemptInfo),
	}
	w := doKiroImport(h, `{"refreshToken":"tok"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestImportKiroToken_AuthDirNotConfigured(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{AuthDir: ""},
		failedAttempts: make(map[string]*attemptInfo),
	}
	w := doKiroImport(h, `{"refreshToken":"tok"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestImportKiroToken_BuilderIdSuccess(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		if req["clientId"] != "cid" || req["clientSecret"] != "csec" || req["grantType"] != "refresh_token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"accessToken":  "at-bid",
			"refreshToken": "rt-bid",
			"expiresIn":    7200,
		})
	}))
	defer mockServer.Close()

	orig := helps.KiroIDCRefreshURLTemplate
	helps.KiroIDCRefreshURLTemplate = mockServer.URL + "/token"
	defer func() { helps.KiroIDCRefreshURLTemplate = orig }()

	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"builder-id","clientId":"cid","clientSecret":"csec"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	store := h.tokenStore.(*memoryAuthStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.items) == 0 {
		t.Fatal("expected auth record to be saved")
	}
	for _, rec := range store.items {
		if rec.Metadata["clientId"] != "cid" {
			t.Errorf("expected clientId cid, got %v", rec.Metadata["clientId"])
		}
		break
	}
}

func TestImportKiroToken_RefreshFailure401_NoSave(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mockServer.Close()

	orig := helps.KiroSocialRefreshURLTemplate
	helps.KiroSocialRefreshURLTemplate = mockServer.URL + "/refreshToken"
	defer func() { helps.KiroSocialRefreshURLTemplate = orig }()

	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"expired","authMethod":"social"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", w.Code, w.Body.String())
	}
	store := h.tokenStore.(*memoryAuthStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.items) != 0 {
		t.Fatal("expected no auth record saved on 401")
	}
}

func TestImportKiroToken_InvalidRegion(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"social","region":"evil.com/path@host"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malicious region, got %d", w.Code)
	}
}

func TestImportKiroToken_InvalidIdcRegion(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"tok","authMethod":"builder-id","clientId":"c","clientSecret":"s","idcRegion":"attacker:8080"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malicious idcRegion, got %d", w.Code)
	}
}

func TestImportKiroToken_WhitespaceOnlyRefreshToken(t *testing.T) {
	h := setupKiroTestHandler(t)
	w := doKiroImport(h, `{"refreshToken":"   ","authMethod":"social"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for whitespace-only refreshToken, got %d", w.Code)
	}
}
