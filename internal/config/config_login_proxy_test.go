package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_OAuthLoginProxy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("config-version: 8\nserver:\n  host: 127.0.0.1\n  port: 18081\nrequests:\n  proxy-url: \"\"\n  proxy-fail-closed: true\noauth:\n  auth-dir: \"~/.cli-proxy-api\"\n  login-proxy: \"http://127.0.0.1:18080\"\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	if cfg.ProxyURL != "" {
		t.Fatalf("ProxyURL = %q, want empty (global must stay empty)", cfg.ProxyURL)
	}
	if !cfg.ProxyFailClosed {
		t.Fatal("ProxyFailClosed = false, want true")
	}
	if cfg.LoginProxyURL != "http://127.0.0.1:18080" {
		t.Fatalf("LoginProxyURL = %q, want http://127.0.0.1:18080 from oauth.login-proxy", cfg.LoginProxyURL)
	}
}
