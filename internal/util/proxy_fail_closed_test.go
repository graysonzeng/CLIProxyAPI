package util

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func mustDiscard(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestSetProxyFailClosedEmptyDoesNotFallbackToDirect(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	client := SetProxy(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: ""}, &http.Client{Timeout: 2 * time.Second})
	resp, err := client.Get(server.URL)
	mustDiscard(resp)
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

func TestSetProxyFailClosedRejectsDirectAndSOCKS5(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	for _, proxyURL := range []string{"direct", "none", "socks5://127.0.0.1:1080"} {
		client := SetProxy(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: proxyURL}, &http.Client{Timeout: 2 * time.Second})
		resp, err := client.Get(server.URL)
		mustDiscard(resp)
		if err == nil || !strings.Contains(err.Error(), "proxy fail-closed") {
			t.Fatalf("proxy %q: error = %v, want proxy fail-closed", proxyURL, err)
		}
	}
}

func TestSetProxyFailClosedDisabledStillFallsBack(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)
	client := SetProxy(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: ""}, &http.Client{Timeout: 2 * time.Second})
	resp, err := client.Get(server.URL)
	mustDiscard(resp)
	if err != nil {
		t.Fatalf("default fail-open empty proxy should still work: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}
