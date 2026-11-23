package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_RequestsProxyFailClosed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	data := []byte("config-version: 8\nserver:\n  host: 127.0.0.1\n  port: 18081\nrequests:\n  proxy-fail-closed: true\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil || !cfg.ProxyFailClosed {
		t.Fatalf("ProxyFailClosed = %v, want true from requests.proxy-fail-closed", cfg)
	}
}
