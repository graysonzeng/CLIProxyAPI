package management

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// Reproduces production management WebUI Antigravity bootstrap under
// requests.proxy-fail-closed=true with an intentionally empty global
// requests.proxy-url and no per-request proxy_url (UI only sends provider +
// is_webui). Without oauth.login-proxy the token exchange must fail closed
// with no direct fallback and no upstream hit.
func TestAntigravityManagementOAuthFailClosedWithoutLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var directHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"should-not-reach","refresh_token":"x","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer upstream.Close()

	originalAgFactory := newAntigravityOAuthService
	newAntigravityOAuthService = func(cfg *config.Config) antigravityOAuthService {
		var sdkCfg config.SDKConfig
		if cfg != nil {
			sdkCfg = cfg.SDKConfig
		}
		return &proxyTestAntigravityOAuthService{
			client:  util.SetProxy(&sdkCfg, &http.Client{}),
			baseURL: upstream.URL,
		}
	}
	defer func() { newAntigravityOAuthService = originalAgFactory }()

	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir: authDir,
		Port:    8317,
		SDKConfig: config.SDKConfig{
			ProxyURL:        "",
			ProxyFailClosed: true,
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	router := gin.New()
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/status", h.GetAuthStatus)

	// Management UI contract: provider + is_webui only (no proxy_url).
	// is_webui is omitted here so the local callback forwarder is not required;
	// login-proxy resolution does not depend on it.
	body := `{"provider":"antigravity"}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/auth-url", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var startResp struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &startResp); err != nil || startResp.State == "" {
		t.Fatalf("decode start: %v body=%s", err, rec.Body.String())
	}
	t.Cleanup(func() { CancelOAuthSession(startResp.State) })

	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, "antigravity", startResp.State, "ag-code", ""); errWrite != nil {
		t.Fatalf("write callback: %v", errWrite)
	}

	status := waitForOAuthStatus(t, router, startResp.State)
	if status.Status != "error" {
		t.Fatalf("status = %#v, want error (fail-closed empty proxy)", status)
	}
	if directHits.Load() != 0 {
		t.Fatalf("direct fallback occurred: directHits=%d", directHits.Load())
	}
	if cfg.ProxyURL != "" {
		t.Fatalf("global ProxyURL mutated to %q", cfg.ProxyURL)
	}
	entries, _ := os.ReadDir(authDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("unexpected credential saved without login proxy: %s", e.Name())
		}
	}
}

func TestAntigravityManagementOAuthUsesConfigLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-secret-pass"))
	var proxyHits, directHits atomic.Int32
	proxyServer := startForwardHTTPProxy(t, &proxyHits, wantAuth, false)
	defer proxyServer.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Via-Session-Proxy") != "1" {
			directHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"ag-access","refresh_token":"ag-refresh","expires_in":3600,"token_type":"Bearer"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"email":"login-proxy-user@example.test"}`))
		case "/project":
			_, _ = w.Write([]byte(`{"project_id":"ag-proj-login-proxy"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	originalAgFactory := newAntigravityOAuthService
	newAntigravityOAuthService = func(cfg *config.Config) antigravityOAuthService {
		var sdkCfg config.SDKConfig
		if cfg != nil {
			sdkCfg = cfg.SDKConfig
		}
		return &proxyTestAntigravityOAuthService{
			client:  util.SetProxy(&sdkCfg, &http.Client{}),
			baseURL: upstream.URL,
		}
	}
	defer func() { newAntigravityOAuthService = originalAgFactory }()

	loginProxyURL := strings.Replace(proxyServer.URL, "http://", "http://proxy-user:proxy-secret-pass@", 1)
	authDir := t.TempDir()
	unrelatedPath := filepath.Join(authDir, "unrelated-openai.json")
	if errWrite := os.WriteFile(unrelatedPath, []byte(`{"type":"openai-compatibility","api_key":"sk-test"}`), 0o600); errWrite != nil {
		t.Fatalf("write unrelated: %v", errWrite)
	}

	cfg := &config.Config{
		AuthDir:       authDir,
		Port:          8317,
		LoginProxyURL: loginProxyURL,
		SDKConfig: config.SDKConfig{
			ProxyURL:        "",
			ProxyFailClosed: true,
		},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	router := gin.New()
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/status", h.GetAuthStatus)

	body := `{"provider":"antigravity"}`
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/auth-url", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var startResp struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &startResp)
	t.Cleanup(func() { CancelOAuthSession(startResp.State) })

	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, "antigravity", startResp.State, "ag-code", ""); errWrite != nil {
		t.Fatalf("write callback: %v", errWrite)
	}

	status := waitForOAuthStatus(t, router, startResp.State)
	if status.Status != "ok" {
		t.Fatalf("status = %#v, want ok with oauth.login-proxy", status)
	}
	if strings.Contains(status.Error, "proxy-secret-pass") || strings.Contains(rec.Body.String(), "proxy-secret-pass") {
		t.Fatalf("response leaked proxy password")
	}
	if proxyHits.Load() != 3 {
		t.Fatalf("proxyHits=%d, want 3 (token+userinfo+project)", proxyHits.Load())
	}
	if directHits.Load() != 0 {
		t.Fatalf("directHits=%d, want 0", directHits.Load())
	}
	if cfg.ProxyURL != "" {
		t.Fatalf("global ProxyURL mutated to %q, want empty", cfg.ProxyURL)
	}

	credPath := filepath.Join(authDir, antigravity.CredentialFileName("login-proxy-user@example.test"))
	raw, errRead := os.ReadFile(credPath)
	if errRead != nil {
		t.Fatalf("read credential: %v", errRead)
	}
	if !strings.Contains(string(raw), `"proxy_url"`) {
		t.Fatalf("new credential missing proxy_url: %s", string(raw))
	}
	var cred map[string]any
	if err := json.Unmarshal(raw, &cred); err != nil {
		t.Fatalf("unmarshal cred: %v", err)
	}
	if got, _ := cred["proxy_url"].(string); got != loginProxyURL {
		t.Fatalf("credential proxy_url = %q, want login proxy", got)
	}
	unrelatedRaw, _ := os.ReadFile(unrelatedPath)
	if strings.Contains(string(unrelatedRaw), "proxy_url") {
		t.Fatalf("unrelated provider credential was modified")
	}
}

func TestAntigravityManagementOAuthRejectsInvalidLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "proxy-userinfo-secret-pass"
	invalidWithUserinfo := []string{
		"://bad",
		"http://proxy-user:" + secret + "@",
		"http://proxy-user:" + secret + "@127.0.0.1:99999",
		"ftp://proxy-user:" + secret + "@127.0.0.1:21",
		"direct",
	}
	for _, badProxy := range invalidWithUserinfo {
		t.Run(badProxy, func(t *testing.T) {
			h := NewHandlerWithoutConfigFilePath(&config.Config{
				AuthDir:       t.TempDir(),
				Port:          8317,
				LoginProxyURL: badProxy,
				SDKConfig:     config.SDKConfig{ProxyFailClosed: true},
			}, nil)
			router := gin.New()
			router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/auth-url", strings.NewReader(`{"provider":"antigravity"}`)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for invalid oauth.login-proxy; body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if body != `{"error":"invalid login-proxy"}` && !strings.Contains(body, `"invalid login-proxy"`) {
				t.Fatalf("unexpected error body: %s", body)
			}
			if strings.Contains(body, secret) || strings.Contains(body, badProxy) || strings.Contains(body, "proxy-user") {
				t.Fatalf("error body leaked proxy userinfo/raw value: %s", body)
			}
		})
	}
}

func TestGetConfigJSONOmitsLoginProxyURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		AuthDir:       t.TempDir(),
		Port:          8317,
		LoginProxyURL: "http://proxy-user:json-leak-secret@127.0.0.1:7890",
		SDKConfig:     config.SDKConfig{ProxyURL: "", ProxyFailClosed: true},
	}, nil)
	router := gin.New()
	router.GET("/config", h.GetConfig)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "login-proxy") || strings.Contains(body, "LoginProxy") || strings.Contains(body, "json-leak-secret") || strings.Contains(body, "proxy-user") {
		t.Fatalf("GetConfig JSON leaked login-proxy: %s", body)
	}
}

func TestSessionProxyURLOverridesConfigLoginProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var hitsLogin, hitsSession, directHits atomic.Int32
	loginProxy := startForwardHTTPProxy(t, &hitsLogin, "", false)
	defer loginProxy.Close()
	sessionProxy := startForwardHTTPProxy(t, &hitsSession, "", false)
	defer sessionProxy.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Via-Session-Proxy") != "1" {
			directHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600,"token_type":"Bearer"}`))
		case "/userinfo":
			_, _ = w.Write([]byte(`{"email":"override@example.test"}`))
		case "/project":
			_, _ = w.Write([]byte(`{"project_id":"p"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	originalAgFactory := newAntigravityOAuthService
	newAntigravityOAuthService = func(cfg *config.Config) antigravityOAuthService {
		var sdkCfg config.SDKConfig
		if cfg != nil {
			sdkCfg = cfg.SDKConfig
		}
		return &proxyTestAntigravityOAuthService{
			client:  util.SetProxy(&sdkCfg, &http.Client{}),
			baseURL: upstream.URL,
		}
	}
	defer func() { newAntigravityOAuthService = originalAgFactory }()

	authDir := t.TempDir()
	cfg := &config.Config{
		AuthDir:       authDir,
		Port:          8317,
		LoginProxyURL: loginProxy.URL,
		SDKConfig:     config.SDKConfig{ProxyFailClosed: true},
	}
	h := NewHandlerWithoutConfigFilePath(cfg, nil)
	router := gin.New()
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/status", h.GetAuthStatus)

	body, _ := json.Marshal(map[string]any{
		"provider":  "antigravity",
		"proxy_url": sessionProxy.URL,
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v8/management/oauth/auth-url", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var startResp struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &startResp)
	t.Cleanup(func() { CancelOAuthSession(startResp.State) })

	if _, errWrite := WriteOAuthCallbackFileForPendingSession(authDir, "antigravity", startResp.State, "code", ""); errWrite != nil {
		t.Fatalf("write callback: %v", errWrite)
	}
	if status := waitForOAuthStatus(t, router, startResp.State); status.Status != "ok" {
		t.Fatalf("status = %#v, want ok", status)
	}
	if hitsSession.Load() != 3 || hitsLogin.Load() != 0 || directHits.Load() != 0 {
		t.Fatalf("hitsSession=%d hitsLogin=%d direct=%d, want 3,0,0", hitsSession.Load(), hitsLogin.Load(), directHits.Load())
	}
	raw, errRead := os.ReadFile(filepath.Join(authDir, antigravity.CredentialFileName("override@example.test")))
	if errRead != nil {
		t.Fatalf("read cred: %v", errRead)
	}
	var cred map[string]any
	_ = json.Unmarshal(raw, &cred)
	if got, _ := cred["proxy_url"].(string); got != sessionProxy.URL {
		t.Fatalf("proxy_url = %q, want session override %q", got, sessionProxy.URL)
	}
}
