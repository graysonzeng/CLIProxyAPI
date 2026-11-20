package util

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// SetProxy configures the provided HTTP client with proxy settings from the configuration.
// It supports SOCKS5, HTTP, and HTTPS proxies. The function modifies the client's transport
// to route requests through the configured proxy server.
// When cfg.ProxyFailClosed is true, empty, direct, non-HTTP, or unusable proxies are
// rejected instead of falling back to the client's existing (often direct) transport.
func SetProxy(cfg *config.SDKConfig, httpClient *http.Client) *http.Client {
	if cfg == nil || httpClient == nil {
		return httpClient
	}

	if cfg.ProxyFailClosed {
		if reason := proxyutil.HTTPFailClosedReason(cfg.ProxyURL); reason != "" {
			log.Errorf("proxy fail-closed: %s", reason)
			httpClient.Transport = proxyutil.NewFailClosedTransport(reason)
			return httpClient
		}
	}

	transport, _, errBuild := proxyutil.BuildHTTPTransport(cfg.ProxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
	}
	if cfg.ProxyFailClosed && (errBuild != nil || transport == nil) {
		reason := "proxy transport build failed"
		if errBuild != nil {
			reason = errBuild.Error()
		}
		log.Errorf("proxy fail-closed: %s", reason)
		httpClient.Transport = proxyutil.NewFailClosedTransport(reason)
		return httpClient
	}
	if transport != nil {
		httpClient.Transport = transport
	}
	return httpClient
}
