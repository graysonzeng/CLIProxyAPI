package claude

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
	"golang.org/x/net/proxy"
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

func unwrapUTLSRoundTripper(t *testing.T, client *http.Client) *utlsRoundTripper {
	t.Helper()
	switch tr := client.Transport.(type) {
	case *utlsRoundTripper:
		return tr
	case httpsOnlyFailClosedTransport:
		rt, ok := tr.base.(*utlsRoundTripper)
		if !ok {
			t.Fatalf("fail-closed base is %T, want *utlsRoundTripper", tr.base)
		}
		return rt
	default:
		t.Fatalf("transport is %T, want *utlsRoundTripper", client.Transport)
		return nil
	}
}

func startConnectProxy(t *testing.T, connects *atomic.Int32, tunnel func(net.Conn)) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("proxy cannot hijack")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		if tunnel != nil {
			tunnel(conn)
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		_, _ = conn.Read(b[:])
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

func TestConfiguredHTTPProxyHTTPSUsesClaudeTLSHook(t *testing.T) {
	t.Parallel()
	for _, strict := range []bool{false, true} {
		strict := strict
		t.Run(fmtStrict(strict), func(t *testing.T) {
			t.Parallel()
			var connects atomic.Int32
			proxy := startConnectProxy(t, &connects, nil)
			client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: strict, ProxyURL: proxy.URL})
			client.Timeout = 2 * time.Second
			rt := unwrapUTLSRoundTripper(t, client)
			if rt.transport.Proxy != nil {
				t.Fatal("HTTPS uTLS transport must not set Transport.Proxy; that skips DialTLSContext after CONNECT")
			}
			called := make(chan struct{}, 1)
			rt.transport.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
				called <- struct{}{}
				return nil, context.Canceled
			}
			resp, _ := client.Get("https://example.invalid/refresh")
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			select {
			case <-called:
			default:
				t.Fatal("configured HTTP proxy bypasses Claude custom TLS hook; standard TLS is substituted")
			}
		})
	}
}

func fmtStrict(strict bool) string {
	if strict {
		return "fail-closed"
	}
	return "fail-open"
}

func TestConfiguredHTTPProxyHTTPSConnectUsesCustomTLSAndSucceeds(t *testing.T) {
	t.Parallel()
	var gotOrder []string
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrder = append([]string{}, r.Header["X-Test-Order"]...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(target.Close)

	var connects atomic.Int32
	proxy := startConnectProxy(t, &connects, func(conn net.Conn) {
		defer conn.Close()
		backend, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			return
		}
		defer backend.Close()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(backend, conn); _ = backend.Close(); close(done) }()
		_, _ = io.Copy(conn, backend)
		<-done
	})

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: proxy.URL})
	client.Timeout = 3 * time.Second
	rt := unwrapUTLSRoundTripper(t, client)
	called := make(chan struct{}, 1)
	if rt.transport.DialTLSContext == nil {
		t.Fatal("production DialTLSContext is not installed")
	}
	rt.transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		called <- struct{}{}
		// httptest certs are untrusted by the production config. Reuse the
		// same proxy dialer, ClientHello, session cache and ordered-header
		// wrap, only skipping verify so the CONNECT tunnel can complete.
		return dialClaudeTLSInsecure(ctx, rt, network, addr)
	}

	req, err := http.NewRequest(http.MethodGet, target.URL+"/api/oauth/profile", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	select {
	case <-called:
	default:
		t.Fatal("successful HTTPS path did not use Claude DialTLSContext")
	}
	if connects.Load() < 1 {
		t.Fatalf("proxy CONNECT count = %d, want at least 1", connects.Load())
	}
	if err != nil {
		t.Fatalf("HTTPS via HTTP proxy CONNECT should succeed with custom TLS: %v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("status = %v, want 200", resp)
	}
	_ = gotOrder
}

func TestFailClosedHTTPSRedirectDowngradeTriggersAfterCONNECT(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{}, 1)
	httpTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	}))
	t.Cleanup(httpTarget.Close)

	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL, http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	var connects atomic.Int32
	proxy := startConnectProxy(t, &connects, func(conn net.Conn) {
		defer conn.Close()
		backend, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			return
		}
		defer backend.Close()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(backend, conn); _ = backend.Close(); close(done) }()
		_, _ = io.Copy(conn, backend)
		<-done
	})

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: true, ProxyURL: proxy.URL})
	client.Timeout = 3 * time.Second
	rt := unwrapUTLSRoundTripper(t, client)
	hook := make(chan struct{}, 2)
	if rt.transport.DialTLSContext == nil {
		t.Fatal("production DialTLSContext is not installed")
	}
	rt.transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		hook <- struct{}{}
		return dialClaudeTLSInsecure(ctx, rt, network, addr)
	}

	resp, err := client.Get(origin.URL + "/refresh")
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	select {
	case <-hook:
	default:
		t.Fatal("downgrade test never reached Claude TLS hook; redirect may have been skipped")
	}
	if connects.Load() < 1 {
		t.Fatalf("CONNECT count = %d, want at least 1", connects.Load())
	}
	if err == nil || !strings.Contains(err.Error(), "proxy fail-closed") {
		t.Fatalf("error = %v, want proxy fail-closed after HTTPS to HTTP redirect", err)
	}
	select {
	case <-reached:
		t.Fatal("fail-closed client followed an HTTPS to HTTP redirect and reached the target")
	default:
	}
}

func TestFailOpenPlainHTTPProxyUsesAbsoluteFormNotConnect(t *testing.T) {
	t.Parallel()
	reached := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached <- struct{}{}
	}))
	t.Cleanup(target.Close)

	var method, uri string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		uri = r.URL.String()
		if r.Method == http.MethodConnect {
			t.Error("plain HTTP target should not use CONNECT")
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)

	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: proxy.URL})
	client.Timeout = 2 * time.Second
	resp, err := client.Get(target.URL + "/plain")
	if resp != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	select {
	case <-reached:
		t.Fatalf("plain HTTP bypassed proxy: error_nil=%t err=%v", err == nil, err)
	default:
	}
	if method == "" {
		t.Fatal("configured HTTP proxy was not contacted for a plain HTTP target")
	}
	if method == http.MethodConnect {
		t.Fatal("plain HTTP used CONNECT")
	}
	if !strings.HasPrefix(uri, "http://") {
		t.Fatalf("proxy request URI %q is not absolute-form", uri)
	}
}

func TestHTTPSProxyURLKeepsClaudeDialTLSContext(t *testing.T) {
	t.Parallel()
	// Do not disable https:// proxy URLs. HTTPS targets must still use the
	// proxy-aware DialTLSContext path, not Transport.Proxy + standard TLS.
	client := NewAnthropicHttpClient(&config.SDKConfig{ProxyFailClosed: false, ProxyURL: "https://127.0.0.1:9"})
	rt := unwrapUTLSRoundTripper(t, client)
	if rt.transport.Proxy != nil {
		t.Fatal("https proxy URL must not set Transport.Proxy on the uTLS transport")
	}
	if rt.transport.DialTLSContext == nil {
		t.Fatal("https proxy URL must keep DialTLSContext")
	}
	if rt.plainHTTP == nil {
		t.Fatal("https proxy URL should still proxy plain HTTP targets")
	}
}

func dialClaudeTLSInsecure(ctx context.Context, rt *utlsRoundTripper, network, addr string) (net.Conn, error) {
	var (
		conn net.Conn
		err  error
	)
	if contextDialer, ok := rt.dialer.(proxy.ContextDialer); ok {
		conn, err = contextDialer.DialContext(ctx, network, addr)
	} else {
		conn, err = rt.dialer.Dial(network, addr)
	}
	if err != nil {
		return nil, err
	}
	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		_ = conn.Close()
		return nil, errSplit
	}
	cfg := newClaudeOAuthTLSConfig(host, rt.sessionCache)
	cfg.InsecureSkipVerify = true
	tlsConn := tls.UClient(conn, cfg, tls.HelloCustom)
	if errPreset := tlsConn.ApplyPreset(claudeOAuthTLSClientHelloSpec()); errPreset != nil {
		_ = tlsConn.Close()
		return nil, errPreset
	}
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		_ = tlsConn.Close()
		return nil, errHandshake
	}
	return httpwire.NewOrderedRequestConn(tlsConn, claudeOAuthRequestHeaderOrder), nil
}
