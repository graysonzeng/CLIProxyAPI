package executor

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

func discardResp(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func TestNewAntigravityHTTPClientFailClosedRejectsEmptyDirectNoneSOCKSAndInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{"", "direct", "none", "socks5://127.0.0.1:1080", "://invalid"}
	for _, raw := range cases {
		raw := raw
		t.Run(nameProxyCase(raw), func(t *testing.T) {
			t.Parallel()
			reached := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached <- struct{}{}
			}))
			t.Cleanup(server.Close)

			cfg := &config.Config{}
			cfg.ProxyFailClosed = true
			auth := antigravityAuthWithIDAndProxy("ag-failclosed-"+nameProxyCase(raw), raw)
			client := newAntigravityHTTPClient(context.Background(), cfg, auth, time.Second)
			resp, err := client.Get(server.URL)
			discardResp(resp)

			hit := false
			select {
			case <-reached:
				hit = true
			default:
			}
			if err == nil || hit {
				t.Fatalf("strict mode escaped proxy %q: error_nil=%t direct_target_hit=%t err=%v", raw, err == nil, hit, err)
			}
			if !strings.Contains(err.Error(), "proxy fail-closed") {
				t.Fatalf("error = %v, want proxy fail-closed", err)
			}
		})
	}
}

func TestNewAntigravityHTTPClientFailClosedProxyFailureDoesNotReachTarget(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	}))
	t.Cleanup(target.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	cfg := &config.Config{}
	cfg.ProxyFailClosed = true
	auth := antigravityAuthWithIDAndProxy("ag-failclosed-proxy-down", proxy.URL)
	client := newAntigravityHTTPClient(context.Background(), cfg, auth, time.Second)
	resp, err := client.Get(target.URL)
	discardResp(resp)

	hit := false
	select {
	case <-reached:
		hit = true
	default:
	}
	if hit {
		t.Fatalf("proxy failure reached the target directly: error_nil=%t err=%v", err == nil, err)
	}
}

func TestNewAntigravityHTTPClientFailClosedDisabledStillAllowsDirect(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	cfg := &config.Config{}
	cfg.ProxyFailClosed = false
	auth := antigravityAuthWithIDAndProxy("ag-failopen-direct", "direct")
	client := newAntigravityHTTPClient(context.Background(), cfg, auth, time.Second)
	resp, err := client.Get(server.URL)
	discardResp(resp)
	if err != nil {
		t.Fatalf("default fail-open direct proxy should still work: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

func nameProxyCase(raw string) string {
	if raw == "" {
		return "empty"
	}
	return raw
}
