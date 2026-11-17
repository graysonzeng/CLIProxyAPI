package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func utlsFailClosedCfg(failClosed bool, proxyURL string) *config.Config {
	cfg := &config.Config{}
	cfg.ProxyFailClosed = failClosed
	cfg.ProxyURL = proxyURL
	return cfg
}

func discardResp(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestNewUtlsHTTPClientFailClosedEmptyDoesNotFallbackToDirect(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	client := NewUtlsHTTPClient(context.Background(), utlsFailClosedCfg(true, ""), nil, time.Second)
	resp, err := client.Get(server.URL)
	discardResp(resp)
	if err == nil {
		t.Fatal("expected fail-closed error for empty proxy, got success via direct")
	}
	if !strings.Contains(err.Error(), "proxy fail-closed") {
		t.Fatalf("error = %v, want proxy fail-closed", err)
	}
	if hits != 0 {
		t.Fatalf("direct fallback reached the server %d times", hits)
	}
}

func TestNewUtlsHTTPClientFailClosedInvalidProxyDoesNotFallback(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	client := NewUtlsHTTPClient(context.Background(), utlsFailClosedCfg(true, "://bad"), nil, time.Second)
	resp, err := client.Get(server.URL)
	discardResp(resp)
	if err == nil || !strings.Contains(err.Error(), "proxy fail-closed") {
		t.Fatalf("error = %v, want proxy fail-closed", err)
	}
	if hits != 0 {
		t.Fatalf("direct fallback reached the server %d times", hits)
	}
}

func TestNewUtlsHTTPClientFailClosedDisabledStillFallsBack(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)
	client := NewUtlsHTTPClient(context.Background(), utlsFailClosedCfg(false, "://bad"), nil, time.Second)
	resp, err := client.Get(server.URL)
	discardResp(resp)
	if err != nil {
		t.Fatalf("default fail-open invalid proxy should still fall back: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}
