package claude

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestNewAnthropicHttpClientFailClosedEmptyDoesNotFallbackToDirect(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: ""})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(server.URL)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
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

func TestNewAnthropicHttpClientFailClosedRejectsDirectAndSOCKS5(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	for _, proxyURL := range []string{"direct", "none", "socks5://127.0.0.1:1080"} {
		client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: proxyURL})
		client.Timeout = 2 * time.Second
		resp, err := client.Get(server.URL)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "proxy fail-closed") {
			t.Fatalf("proxy %q: error = %v, want proxy fail-closed", proxyURL, err)
		}
	}
}

func TestNewAnthropicHttpClientFailClosedDisabledStillFallsBack(t *testing.T) {
	t.Parallel()
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		hits++
	}))
	t.Cleanup(server.Close)
	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: ""})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(server.URL)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("default fail-open empty proxy should still work: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1", hits)
	}
}

func TestNewAnthropicHttpClientFailClosedHTTPDoesNotBypassProxy(t *testing.T) {
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

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: proxy.URL})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(target.URL)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	hit := false
	select {
	case <-reached:
		hit = true
	default:
	}
	if hit {
		t.Fatalf("strict valid proxy bypassed for http scheme: error_nil=%t err=%v", err == nil, err)
	}
	if err == nil || !strings.Contains(err.Error(), "proxy fail-closed") || !strings.Contains(strings.ToLower(err.Error()), "non-https") {
		t.Fatalf("error = %v, want proxy fail-closed refusing non-https", err)
	}
}

func TestNewAnthropicHttpClientFailOpenHTTPUsesConfiguredProxy(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	}))
	t.Cleanup(target.Close)

	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	// Default (non-strict) mode must still send plain HTTP through a configured
	// HTTP proxy. Before the Transport.Proxy fix, DialTLSContext was unused for
	// http:// and the request reached the target directly.
	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: proxy.URL})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(target.URL)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	hit := false
	select {
	case <-reached:
		hit = true
	default:
	}
	if hit {
		t.Fatalf("configured HTTP proxy bypassed in default mode: error_nil=%t err=%v proxy_hits=%d", err == nil, err, proxyHits)
	}
	if proxyHits < 1 {
		t.Fatalf("proxy was not contacted: proxy_hits=%d error_nil=%t err=%v", proxyHits, err == nil, err)
	}
}

func TestFailClosedHTTPSRedirectRejectsDowngrade(t *testing.T) {
	t.Parallel()
	httpsReq, err := http.NewRequest(http.MethodGet, "https://example.invalid/next", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := failClosedHTTPSRedirect(httpsReq, nil); err != nil {
		t.Fatalf("https redirect should be allowed: %v", err)
	}

	httpReq, err := http.NewRequest(http.MethodGet, "http://example.invalid/downgrade", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := failClosedHTTPSRedirect(httpReq, []*http.Request{httpsReq}); err == nil || !strings.Contains(err.Error(), "proxy fail-closed") {
		t.Fatalf("error = %v, want proxy fail-closed for http downgrade", err)
	}
}

func TestNewAnthropicHttpClientFailClosedRejectsHTTPRedirectDowngrade(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{}, 1)
	httpTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	}))
	t.Cleanup(httpTarget.Close)

	// A dummy HTTPS origin that only exists to request a redirect to HTTP.
	// The client may fail the TLS handshake; the assertion is that the
	// HTTP downgrade target is never reached.
	httpsOrigin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL, http.StatusFound)
	}))
	t.Cleanup(httpsOrigin.Close)

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: proxy.URL})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(httpsOrigin.URL)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_ = err
	hit := false
	select {
	case <-reached:
		hit = true
	default:
	}
	if hit {
		t.Fatal("fail-closed client followed an HTTPS to HTTP redirect and reached the target directly")
	}
}
