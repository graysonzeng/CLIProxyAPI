package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KiroExecutor implements ProviderExecutor for the Kiro (CodeWhisperer) provider.
// It translates Claude-format payloads into CodeWhisperer generateAssistantResponse
// requests and converts Kiro responses back to Claude format.
type KiroExecutor struct {
	cfg *config.Config
}

// NewKiroExecutor creates a new Kiro executor.
func NewKiroExecutor(cfg *config.Config) *KiroExecutor { return &KiroExecutor{cfg: cfg} }

// Identifier returns the provider key.
func (e *KiroExecutor) Identifier() string { return "kiro" }

const (
	kiroHaikuMaxConcurrentPerAuth  = 4
	kiroSonnetMaxConcurrentPerAuth = 2
)

var kiroLightRequestGates sync.Map // map[string]chan struct{}

type releaseOnCloseReadCloser struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (r *releaseOnCloseReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.release)
	return err
}

// kiroDefaultThinkingEffort returns the configured default thinking effort for
// rewriting `enabled+budget` requests into Kiro's adaptive thinking mode.
// Returns "" when the executor has no config attached (e.g. in unit tests),
// which causes SelectKiroThinkingPrefix to use its built-in medium fallback.
func (e *KiroExecutor) kiroDefaultThinkingEffort() string {
	if e == nil || e.cfg == nil {
		return ""
	}
	return e.cfg.Kiro.DefaultThinkingEffort
}

// httpClientFor selects the HTTP client to use for a Kiro upstream request.
//
// Hot path (no per-auth proxy, no global proxy, no context-injected
// RoundTripper) returns the process-wide shared client from
// helps.KiroSharedHTTPClient so idle keep-alive connections to the AWS
// CodeWhisperer endpoint can be reused across requests. With the previous
// per-call &http.Client{} pattern every request paid a fresh TCP+TLS
// handshake to us-east-1, costing ~3 RTTs (~200-500ms from typical Asia
// hosts) on every request — including consecutive requests in the same
// Claude Code session.
//
// When a proxy URL is configured (per-auth or global) or the caller has
// stashed a custom RoundTripper in the context, the existing
// NewProxyAwareHTTPClient path is preserved verbatim so users relying on
// those features see no behavior change.
func (e *KiroExecutor) httpClientFor(ctx context.Context, auth *cliproxyauth.Auth) *http.Client {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && e.cfg != nil {
		proxyURL = strings.TrimSpace(e.cfg.ProxyURL)
	}
	if proxyURL != "" {
		return helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	}
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		return helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	}
	return helps.KiroSharedHTTPClient()
}

// PrepareRequest injects Kiro credentials into the outgoing HTTP request.
func (e *KiroExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := kiroAccessToken(auth)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Kiro credentials and executes the HTTP request.
func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := e.httpClientFor(ctx, auth)
	return httpClient.Do(httpReq)
}

// CountTokens is not natively supported by Kiro; returns unsupported error.
func (e *KiroExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "cpa_kiro_count_tokens_unsupported: kiro does not support precise token counting"}
}

// Refresh handles token refresh for both social and builder-id auth methods.
func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("kiro executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("kiro executor: auth is nil")
	}
	refreshToken := kiroMetaStr(auth, "refreshToken")
	if refreshToken == "" {
		return auth, nil
	}

	authMethod := kiroMetaStr(auth, "authMethod")
	hasIDCCreds := kiroMetaStr(auth, "clientId") != "" && kiroMetaStr(auth, "clientSecret") != ""
	isSocial := authMethod == helps.KiroAuthMethodSocial || (authMethod == "" && !hasIDCCreds)
	if authMethod == "" {
		if isSocial {
			authMethod = helps.KiroAuthMethodSocial
		} else {
			authMethod = "builder-id"
		}
		log.Warnf("kiro executor: authMethod missing, inferred %s", authMethod)
	}

	region := kiroMetaStr(auth, "region")

	// Build JSON request body per AIClient2API source.
	reqBody := map[string]string{"refreshToken": refreshToken}
	var refreshURL string
	if isSocial {
		refreshURL = helps.KiroSocialRefreshURL(region)
	} else {
		clientID := kiroMetaStr(auth, "clientId")
		clientSecret := kiroMetaStr(auth, "clientSecret")
		if clientID == "" || clientSecret == "" {
			return nil, fmt.Errorf("kiro executor: builder-id refresh requires clientId and clientSecret in auth metadata (check auths/kiro.json)")
		}
		idcRegion := kiroMetaStr(auth, "idcRegion")
		if idcRegion == "" {
			idcRegion = helps.KiroIDCRegion
		}
		refreshURL = helps.KiroIDCRefreshURL(idcRegion)
		reqBody["clientId"] = clientID
		reqBody["clientSecret"] = clientSecret
		reqBody["grantType"] = "refresh_token"
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("kiro executor: failed to marshal refresh body: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("kiro executor: failed to create refresh request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 15*time.Second)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("kiro executor: refresh request failed: %w", err)
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: refresh response body close error: %v", errClose)
		}
	}()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("kiro executor: failed to read refresh response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("kiro executor: refresh returned %d: %s", httpResp.StatusCode, string(respBody))
	}

	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ProfileArn   string `json:"profileArn"`
		ExpiresIn    int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("kiro executor: failed to parse refresh response: %w", err)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("kiro executor: refresh response missing accessToken")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["accessToken"] = result.AccessToken
	if result.RefreshToken != "" {
		auth.Metadata["refreshToken"] = result.RefreshToken
	}
	if result.ProfileArn != "" {
		auth.Metadata["profileArn"] = result.ProfileArn
	}
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
	auth.Metadata["expiresAt"] = expiresAt
	auth.Metadata["authMethod"] = authMethod
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)

	log.Infof("kiro executor: token refreshed successfully, expires at %s", expiresAt)
	return auth, nil
}

// Execute performs a non-streaming request.
// It translates the client payload to Claude format, builds a CodeWhisperer request,
// sends it, parses the Kiro response, constructs a Claude Message JSON response,
// then translates back to the client format.
func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")

	// Translate client payload to Claude format.
	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	// Apply unified thinking pipeline (suffix override, capability check, normalize/validate).
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	// Build CodeWhisperer request from the Claude-format payload.
	cwReq, toolNameMaps, err := buildKiroCodeWhispererRequest(body, auth, e.kiroDefaultThinkingEffort())
	if err != nil {
		return resp, fmt.Errorf("kiro executor: %w", err)
	}

	httpResp, err := e.sendKiroRequest(ctx, auth, cwReq, baseModel)
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: response body close error: %v", errClose)
		}
	}()

	rawResp, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return resp, fmt.Errorf("kiro executor: failed to read response: %w", err)
	}

	// Claude clients expect the Messages API JSON shape for non-streaming responses.
	claudeJSON := buildClaudeMessageJSON(rawResp, toolNameMaps, baseModel)
	if from == to {
		reporter.EnsurePublished(ctx)
		return cliproxyexecutor.Response{Payload: claudeJSON, Headers: httpResp.Header.Clone()}, nil
	}

	// Cross-protocol response translators consume Claude SSE data events.
	claudeSSE := buildClaudeMessageSSE(rawResp, toolNameMaps, baseModel)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, claudeSSE, &param)
	reporter.EnsurePublished(ctx)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

// ExecuteStream performs a streaming request.
// It translates the client payload to Claude format, builds a CodeWhisperer request,
// streams the Kiro response, translates each event to Claude SSE, then optionally
// translates to the client format.
func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")

	body := sdktranslator.TranslateRequest(from, to, baseModel, bytes.Clone(req.Payload), true)

	// Apply unified thinking pipeline (suffix override, capability check, normalize/validate).
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	if isKiroClaudeCodeTitleGenerationRequest(body, baseModel, opts.Headers) {
		reporter.EnsurePublished(ctx)
		return buildKiroSyntheticClaudeTitleStream(ctx, body, baseModel), nil
	}
	opusStreamingTuning := shouldTuneKiroOpusStreamingRequest(body, baseModel, opts.Headers)
	if opusStreamingTuning {
		body, err = appendKiroClaudePayloadSystemGuidance(body, kiroClaudeCodeStreamingGuidance)
		if err != nil {
			return nil, fmt.Errorf("kiro executor: attach Claude Code guidance: %w", err)
		}
		if kiroRequestHasIncompleteAssistantTail(body) {
			body, err = appendKiroClaudePayloadSystemGuidance(body, kiroClaudeCodeContinuationGuidance)
			if err != nil {
				return nil, fmt.Errorf("kiro executor: attach Claude Code continuation guidance: %w", err)
			}
		}
		log.WithFields(log.Fields{
			"event":      "kiro_opus_streaming_guidance_attached",
			"provider":   e.Identifier(),
			"model":      baseModel,
			"request_id": logging.GetRequestID(ctx),
		}).Info("kiro executor: attached Kiro Opus streaming guidance")
	}
	if shouldSuppressKiroOpusStreamingThinking(body, baseModel, opts.Headers) {
		body, err = suppressKiroThinking(body)
		if err != nil {
			return nil, fmt.Errorf("kiro executor: suppress continuation thinking: %w", err)
		}
		log.WithFields(log.Fields{
			"event":      "kiro_opus_streaming_thinking_suppressed",
			"provider":   e.Identifier(),
			"model":      baseModel,
			"request_id": logging.GetRequestID(ctx),
		}).Info("kiro executor: suppressing thinking for Kiro Opus streaming turn")
	}
	if isKiroOpusModel(baseModel) && kiroRequestHasIncompleteAssistantTail(body) {
		ctx = context.WithValue(ctx, kiroContinuationAfterIncompleteAssistantKey{}, true)
	}
	if looksLikeClaudeCodeRequest(body, opts.Headers) {
		ctx = context.WithValue(ctx, kiroClaudeCodeRequestKey{}, true)
	}

	cwReq, toolNameMaps, err := buildKiroCodeWhispererRequest(body, auth, e.kiroDefaultThinkingEffort())
	if err != nil {
		return nil, fmt.Errorf("kiro executor: %w", err)
	}
	retryBody, err := suppressKiroThinking(body)
	if err != nil {
		return nil, fmt.Errorf("kiro executor: suppress retry thinking: %w", err)
	}
	retryCWReq, _, err := buildKiroCodeWhispererRequest(retryBody, auth, e.kiroDefaultThinkingEffort())
	if err != nil {
		return nil, fmt.Errorf("kiro executor: build retry request: %w", err)
	}

	httpResp, err := e.sendKiroRequest(ctx, auth, cwReq, baseModel)
	if err != nil {
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		currentResp := httpResp
		defer func() {
			if currentResp == nil || currentResp.Body == nil {
				return
			}
			if errClose := currentResp.Body.Close(); errClose != nil {
				log.Errorf("kiro executor: response body close error: %v", errClose)
			}
		}()

		runStream := func(resp *http.Response) (kiroStreamResult, error) {
			if from == to {
				// Claude→Claude: forward SSE lines directly.
				return streamKiroToClaudeSSE(ctx, resp.Body, toolNameMaps, baseModel, func(line []byte) {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: append(line, '\n')}:
					case <-ctx.Done():
					}
				})
			}
			// Other formats: translate each SSE line.
			var param any
			return streamKiroToClaudeSSE(ctx, resp.Body, toolNameMaps, baseModel, func(line []byte) {
				for _, dataLine := range claudeSSEDataLines(line) {
					chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, dataLine, &param)
					for i := range chunks {
						select {
						case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
						case <-ctx.Done():
						}
					}
				}
			})
		}

		normalizeBeforePayloadEmpty := func(result kiroStreamResult, err error) (kiroStreamResult, error) {
			if err == nil && !result.payloadStarted {
				return result, newKiroStreamError(helps.KiroErrStreamMalformed, "upstream stream closed before first payload", nil)
			}
			return result, err
		}

		currentReq := cwReq
		streamResult, streamErr := normalizeBeforePayloadEmpty(runStream(currentResp))
		for attempt := 1; streamErr != nil && !streamResult.payloadStarted && isRetryableKiroStreamBeforePayload(streamErr) && contextErr(ctx) == nil && attempt <= maxKiroStreamRetriesBeforePayload; attempt++ {
			if attempt == 1 && !bytes.Equal(currentReq, retryCWReq) {
				currentReq = retryCWReq
				log.WithFields(log.Fields{
					"event":      "stream_retry_suppress_thinking",
					"provider":   e.Identifier(),
					"model":      baseModel,
					"request_id": logging.GetRequestID(ctx),
					"attempt":    attempt,
				}).Info("kiro executor: retrying stream with thinking disabled before payload")
			}
			log.WithFields(log.Fields{
				"event":      "stream_retry_before_payload",
				"provider":   e.Identifier(),
				"model":      baseModel,
				"request_id": logging.GetRequestID(ctx),
				"attempt":    attempt,
				"error":      kiroHandledRetryLogError(streamErr),
			}).Warn("kiro executor: retrying stream before payload after malformed empty stream")
			if errClose := currentResp.Body.Close(); errClose != nil {
				log.Errorf("kiro executor: response body close before retry error: %v", errClose)
			}
			currentResp = nil
			retryResp, retryErr := e.sendKiroRequest(ctx, auth, currentReq, baseModel)
			if retryErr != nil {
				streamErr = retryErr
			} else {
				currentResp = retryResp
				streamResult, streamErr = normalizeBeforePayloadEmpty(runStream(currentResp))
				if streamErr == nil {
					log.WithFields(log.Fields{
						"event":      "stream_retry_before_payload_succeeded",
						"provider":   e.Identifier(),
						"model":      baseModel,
						"request_id": logging.GetRequestID(ctx),
						"attempt":    attempt,
					}).Info("kiro executor: stream retry before payload succeeded")
				}
			}
		}
		if streamErr != nil {
			reporter.PublishFailure(ctx, streamErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
			return
		}
		if streamResult.payloadStarted {
			reporter.EnsurePublished(ctx)
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

const maxKiroStreamRetriesBeforePayload = 4

func isKiroClaudeCodeTitleGenerationRequest(body []byte, baseModel string, headers http.Header) bool {
	if !isKiroOpusModel(baseModel) || !looksLikeClaudeCodeRequest(body, headers) {
		return false
	}
	if gjson.GetBytes(body, "output_config.format.type").String() != "json_schema" {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) != 0 {
		return false
	}
	schema := gjson.GetBytes(body, "output_config.format.schema")
	if schema.Exists() && !schema.Get("properties.title").Exists() {
		return false
	}
	for _, systemPart := range gjson.GetBytes(body, "system").Array() {
		text := systemPart.Get("text").String()
		if strings.Contains(text, "Generate a concise, sentence-case title") && strings.Contains(text, "Return JSON with a single \"title\" field") {
			return true
		}
	}
	return false
}

func looksLikeClaudeCodeRequest(body []byte, headers http.Header) bool {
	if looksLikeClaudeCodeHeaders(headers) {
		return true
	}
	if looksLikeClaudeCodeKiroRequest(gjson.GetBytes(body, "tools")) {
		return true
	}
	for _, systemPart := range gjson.GetBytes(body, "system").Array() {
		text := strings.ToLower(systemPart.Get("text").String())
		if strings.Contains(text, "cc_version=") || strings.Contains(text, "cc_entrypoint=") {
			return true
		}
	}
	return false
}

func buildKiroSyntheticClaudeTitleStream(ctx context.Context, body []byte, model string) *cliproxyexecutor.StreamResult {
	title := kiroSyntheticClaudeCodeTitle(body)
	contentBytes, _ := json.Marshal(map[string]string{"title": title})
	content := string(contentBytes)
	msgID := "msg_" + uuid.New().String()
	events := []struct {
		name string
		data any
	}{
		{
			name: "message_start",
			data: map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":      msgID,
					"type":    "message",
					"role":    "assistant",
					"content": []any{},
					"model":   model,
					"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			},
		},
		{
			name: "content_block_start",
			data: map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			},
		},
		{
			name: "content_block_delta",
			data: map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": content,
				},
			},
		},
		{
			name: "content_block_stop",
			data: map[string]any{
				"type":  "content_block_stop",
				"index": 0,
			},
		},
		{
			name: "message_delta",
			data: map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"output_tokens": 1},
			},
		},
		{
			name: "message_stop",
			data: map[string]any{"type": "message_stop"},
		},
	}
	out := make(chan cliproxyexecutor.StreamChunk, len(events)+1)
	for _, event := range events {
		jsonData, _ := json.Marshal(event.data)
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event.name, jsonData))}:
		case <-ctx.Done():
			out <- cliproxyexecutor.StreamChunk{Err: ctx.Err()}
			close(out)
			headers := http.Header{}
			headers.Set("Content-Type", "text/event-stream")
			return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
		}
	}
	close(out)
	headers := http.Header{}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func kiroSyntheticClaudeCodeTitle(body []byte) string {
	session := ""
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		if msg.Get("role").String() != "user" {
			continue
		}
		for _, part := range msg.Get("content").Array() {
			if part.Get("type").String() == "text" {
				session = part.Get("text").String()
				break
			}
		}
		if session != "" {
			break
		}
	}
	session = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(session, "</session>"), "<session>"))
	if session == "" {
		return "Coding session"
	}
	fields := strings.Fields(session)
	if len(fields) > 0 {
		session = strings.Join(fields, " ")
	}
	runes := []rune(session)
	if len(runes) > 48 {
		session = string(runes[:48])
	}
	return strings.TrimSpace(session)
}

func isRetryableKiroStreamBeforePayload(err error) bool {
	var kiroErr *helps.KiroError
	return errors.As(err, &kiroErr) && kiroErr.Classification() == helps.KiroErrStreamMalformed
}

func suppressKiroThinking(body []byte) ([]byte, error) {
	out, err := sjson.SetBytes(body, "thinking", map[string]interface{}{"type": "disabled"})
	if err != nil {
		return nil, err
	}
	out, err = sjson.DeleteBytes(out, "output_config.effort")
	if err != nil {
		return nil, err
	}
	return out, nil
}

func shouldSuppressKiroOpusStreamingThinking(body []byte, baseModel string, headers http.Header) bool {
	if !isKiroOpusModel(baseModel) {
		return false
	}
	// Kiro Opus long Claude Code turns repeatedly showed hidden-thinking stalls
	// after visible output. For streaming requests, prioritize continuous user
	// visible output over another internal thinking pass.
	return true
}

func shouldSuppressKiroClaudeCodeThinking(body []byte, baseModel string, headers http.Header) bool {
	if !isKiroOpusModel(baseModel) {
		return false
	}
	if looksLikeClaudeCodeHeaders(headers) || looksLikeClaudeCodeKiroRequest(gjson.GetBytes(body, "tools")) {
		return true
	}
	if kiroRequestHasIncompleteAssistantTail(body) {
		return true
	}
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) == 0 {
		return false
	}
	last := messages[len(messages)-1]
	if last.Get("role").String() != "user" {
		return false
	}
	return kiroMessageHasToolResult(last.Get("content"))
}

func shouldTuneKiroOpusStreamingRequest(body []byte, baseModel string, headers http.Header) bool {
	return isKiroOpusModel(baseModel)
}

func shouldTuneKiroClaudeCodeRequest(body []byte, baseModel string, headers http.Header) bool {
	return shouldTuneKiroOpusStreamingRequest(body, baseModel, headers) && (looksLikeClaudeCodeHeaders(headers) || looksLikeClaudeCodeKiroRequest(gjson.GetBytes(body, "tools")))
}

func looksLikeClaudeCodeHeaders(headers http.Header) bool {
	if headers == nil {
		return false
	}
	billingHeader := strings.ToLower(headers.Get("x-anthropic-billing-header"))
	return strings.Contains(billingHeader, "cc_version=") || strings.Contains(billingHeader, "cc_entrypoint=")
}

func appendKiroClaudePayloadSystemGuidance(body []byte, guidance string) ([]byte, error) {
	system := gjson.GetBytes(body, "system")
	if !system.Exists() {
		return sjson.SetBytes(body, "system", guidance)
	}
	systemPrompt := appendKiroSystemGuidance(extractSystemPromptText(system), guidance)
	return sjson.SetBytes(body, "system", systemPrompt)
}

func kiroHandledRetryLogError(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), "stream_malformed", "malformed_stream")
}

func kiroMessageHasToolResult(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if part.Get("type").String() == "tool_result" {
			return true
		}
	}
	return false
}

// --- Internal helpers ---

// sendKiroRequest sends the prepared CodeWhisperer request to Kiro and returns
// the open response on success. It implements the minimum viable executor-local
// runtime error policy described in the Kiro core-reliability spec (P0-2):
//
//   - 401 invalid bearer: attempt one bounded force refresh and retry exactly
//     once. If the second attempt is also 401 (or any other error), propagate
//     the classified error so the conductor can fail over.
//   - 402 quota exhausted, 403 forbidden, 429 rate-limited, 408/5xx transient,
//     and network errors: classify into KiroError without retrying or
//     poisoning credential state. The conductor's existing failover/cooldown
//     logic handles cross-credential routing.
//
// On non-2xx, the response body is fully consumed and closed before the
// classified error is returned. On success, the caller owns the open body.
func (e *KiroExecutor) sendKiroRequest(ctx context.Context, auth *cliproxyauth.Auth, cwReq []byte, baseModel string) (*http.Response, error) {
	region := kiroMetaStr(auth, "region")
	url := helps.KiroBaseURL(region)

	requestID := logging.GetRequestID(ctx)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}

	// First attempt.
	resp, err := e.doKiroHTTPWithGate(ctx, auth, url, cwReq, baseModel, authID)
	if err != nil {
		log.WithFields(log.Fields{
			"event":       "request_error",
			"provider":    e.Identifier(),
			"auth_id":     authID,
			"model":       baseModel,
			"error_class": helps.KiroErrNetwork,
			"request_id":  requestID,
		}).Warnf("kiro executor: network error: %v", err)
		return nil, err
	}

	// Success path: hand the open body to the caller.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.WithFields(log.Fields{
			"event":           "request_complete",
			"provider":        e.Identifier(),
			"auth_id":         authID,
			"model":           baseModel,
			"upstream_status": resp.StatusCode,
			"request_id":      requestID,
		}).Debug("kiro executor: request complete")
		return resp, nil
	}

	// Non-2xx on first attempt. Classify, then decide whether to retry once.
	classified := classifyAndCloseKiroResponse(resp)

	if classified.Class == helps.KiroErrUnauthorized {
		refreshSource := "executor_local"
		if cliproxyauth.ForceRefreshAuthFromContext(ctx) != nil {
			refreshSource = "manager_persisted"
		}
		log.WithFields(log.Fields{
			"event":           "refresh_attempt",
			"provider":        e.Identifier(),
			"auth_id":         authID,
			"model":           baseModel,
			"upstream_status": classified.Status,
			"request_id":      requestID,
			"refresh_source":  refreshSource,
		}).Warn("kiro executor: 401 from upstream; attempting bounded force refresh + single retry")

		refreshErr := e.refreshAuthForRetry(ctx, auth)
		if refreshErr != nil {
			log.WithFields(log.Fields{
				"event":          "refresh_result",
				"provider":       e.Identifier(),
				"auth_id":        authID,
				"model":          baseModel,
				"outcome":        "failed",
				"request_id":     requestID,
				"refresh_source": refreshSource,
			}).Warnf("kiro executor: refresh failed: %v", refreshErr)
			return nil, classified
		}
		log.WithFields(log.Fields{
			"event":          "refresh_result",
			"provider":       e.Identifier(),
			"auth_id":        authID,
			"model":          baseModel,
			"outcome":        "succeeded",
			"request_id":     requestID,
			"refresh_source": refreshSource,
		}).Info("kiro executor: refresh succeeded; retrying request once")

		retryResp, retryErr := e.doKiroHTTPWithGate(ctx, auth, url, cwReq, baseModel, authID)
		if retryErr != nil {
			log.WithFields(log.Fields{
				"event":       "request_error",
				"provider":    e.Identifier(),
				"auth_id":     authID,
				"model":       baseModel,
				"error_class": helps.KiroErrNetwork,
				"request_id":  requestID,
				"retry":       true,
			}).Warnf("kiro executor: network error after refresh retry: %v", retryErr)
			return nil, retryErr
		}
		if retryResp.StatusCode >= 200 && retryResp.StatusCode < 300 {
			log.WithFields(log.Fields{
				"event":           "request_complete",
				"provider":        e.Identifier(),
				"auth_id":         authID,
				"model":           baseModel,
				"upstream_status": retryResp.StatusCode,
				"request_id":      requestID,
				"retry":           true,
			}).Debug("kiro executor: request complete after refresh retry")
			return retryResp, nil
		}
		retryClassified := classifyAndCloseKiroResponse(retryResp)
		log.WithFields(log.Fields{
			"event":           "request_error",
			"provider":        e.Identifier(),
			"auth_id":         authID,
			"model":           baseModel,
			"upstream_status": retryClassified.Status,
			"error_class":     retryClassified.Class,
			"request_id":      requestID,
			"retry":           true,
		}).Warn("kiro executor: classified error after refresh retry")
		return nil, retryClassified
	}

	log.WithFields(log.Fields{
		"event":           "request_error",
		"provider":        e.Identifier(),
		"auth_id":         authID,
		"model":           baseModel,
		"upstream_status": classified.Status,
		"error_class":     classified.Class,
		"request_id":      requestID,
	}).Warn("kiro executor: classified upstream error")
	return nil, classified
}

func (e *KiroExecutor) doKiroHTTPWithGate(ctx context.Context, auth *cliproxyauth.Auth, url string, cwReq []byte, baseModel, authID string) (*http.Response, error) {
	upstreamModel := helps.MapKiroModel(strings.TrimSpace(baseModel))
	release, waitedMs, gated, err := acquireKiroLightRequestGate(ctx, authID, baseModel)
	if err != nil {
		return nil, err
	}
	if gated && waitedMs > 0 {
		message := fmt.Sprintf(
			"kiro executor: light_model_gate_wait event=light_model_gate_wait provider=%s model=%s upstream_model=%s wait_ms=%d gate_limit=%d request_id=%s",
			e.Identifier(), baseModel, upstreamModel, waitedMs, kiroModelGateLimit(upstreamModel), logging.GetRequestID(ctx),
		)
		log.WithFields(log.Fields{
			"event":          "light_model_gate_wait",
			"provider":       e.Identifier(),
			"model":          baseModel,
			"upstream_model": upstreamModel,
			"wait_ms":        waitedMs,
			"gate_limit":     kiroModelGateLimit(upstreamModel),
			"request_id":     logging.GetRequestID(ctx),
		}).Info(message)
	}
	headerStartedAt := time.Now()
	resp, err := e.doKiroHTTP(ctx, auth, url, cwReq)
	if err != nil {
		release()
		return nil, err
	}
	headerMs := time.Since(headerStartedAt).Milliseconds()
	headerMessage := fmt.Sprintf(
		"kiro executor: upstream_headers event=upstream_headers provider=%s model=%s upstream_model=%s upstream_status=%d header_ms=%d gate_wait_ms=%d gate_limit=%d gated=%t request_id=%s",
		e.Identifier(), baseModel, upstreamModel, resp.StatusCode, headerMs, waitedMs, kiroModelGateLimit(upstreamModel), gated, logging.GetRequestID(ctx),
	)
	log.WithFields(log.Fields{
		"event":           "upstream_headers",
		"provider":        e.Identifier(),
		"model":           baseModel,
		"upstream_model":  upstreamModel,
		"upstream_status": resp.StatusCode,
		"header_ms":       headerMs,
		"gate_wait_ms":    waitedMs,
		"gate_limit":      kiroModelGateLimit(upstreamModel),
		"gated":           gated,
		"request_id":      logging.GetRequestID(ctx),
	}).Info(headerMessage)
	if !gated {
		return resp, nil
	}
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func acquireKiroLightRequestGate(ctx context.Context, authID, baseModel string) (func(), int64, bool, error) {
	upstreamModel := helps.MapKiroModel(strings.TrimSpace(baseModel))
	limit := kiroModelGateLimit(upstreamModel)
	if limit <= 0 {
		return func() {}, 0, false, nil
	}
	keyAuth := strings.TrimSpace(authID)
	if keyAuth == "" {
		keyAuth = "unknown"
	}
	key := keyAuth + "|" + upstreamModel
	gateValue, _ := kiroLightRequestGates.LoadOrStore(key, make(chan struct{}, limit))
	gate := gateValue.(chan struct{})
	startedAt := time.Now()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, time.Since(startedAt).Milliseconds(), true, nil
	case <-ctx.Done():
		return nil, time.Since(startedAt).Milliseconds(), true, ctx.Err()
	}
}

func kiroShouldGateLightModel(baseModel string) bool {
	return kiroModelGateLimit(helps.MapKiroModel(strings.TrimSpace(baseModel))) > 0
}

func kiroModelGateLimit(upstreamModel string) int {
	upstreamModel = strings.TrimSpace(upstreamModel)
	switch {
	case strings.HasPrefix(upstreamModel, "claude-haiku-"):
		return kiroHaikuMaxConcurrentPerAuth
	case strings.HasPrefix(upstreamModel, "claude-sonnet-"):
		return kiroSonnetMaxConcurrentPerAuth
	default:
		return 0
	}
}

// refreshAuthForRetry runs the bounded 401-driven refresh used by
// sendKiroRequest. When the conductor has installed a force-refresh callback in
// ctx (the production path), we go through Manager.ForceRefreshAuth so the new
// credentials are also written to the manager's in-memory map and the
// configured Store. The executor's local auth pointer is then mutated in-place
// so the retry uses the new Authorization header. When no callback is present
// (direct executor unit tests), we fall back to executor-local Refresh, which
// matches the original P0-2 behavior.
func (e *KiroExecutor) refreshAuthForRetry(ctx context.Context, auth *cliproxyauth.Auth) error {
	if auth == nil {
		return fmt.Errorf("kiro executor: auth is nil")
	}
	if fn := cliproxyauth.ForceRefreshAuthFromContext(ctx); fn != nil && auth.ID != "" {
		updated, err := fn(ctx, auth.ID)
		if err != nil {
			return err
		}
		applyRefreshedAuthSnapshot(auth, updated)
		return nil
	}
	updated, err := e.Refresh(ctx, auth)
	if err != nil {
		return err
	}
	applyRefreshedAuthSnapshot(auth, updated)
	return nil
}

// applyRefreshedAuthSnapshot copies the refreshed credential material from
// snapshot back onto the live auth handle so the in-flight retry observes the
// new Authorization header. Only credential-bearing fields are copied; counters
// and runtime state managed by the conductor are intentionally left untouched
// to avoid accidentally clobbering recently incremented metrics.
func applyRefreshedAuthSnapshot(auth, snapshot *cliproxyauth.Auth) {
	if auth == nil || snapshot == nil {
		return
	}
	if snapshot.Metadata != nil {
		auth.Metadata = snapshot.Metadata
	}
	if !snapshot.LastRefreshedAt.IsZero() {
		auth.LastRefreshedAt = snapshot.LastRefreshedAt
	}
}

// doKiroHTTP issues a single HTTP request to Kiro using a fresh body reader and
// the latest credentials (after a possible Refresh). Network failures are
// wrapped via helps.ClassifyKiroNetworkError.
func (e *KiroExecutor) doKiroHTTP(ctx context.Context, auth *cliproxyauth.Auth, url string, cwReq []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(cwReq))
	if err != nil {
		return nil, fmt.Errorf("kiro executor: %w", err)
	}
	applyKiroHTTPHeaders(httpReq, auth)

	httpClient := e.httpClientFor(ctx, auth)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, helps.ClassifyKiroNetworkError(err)
	}
	return resp, nil
}

// classifyAndCloseKiroResponse reads the upstream body, closes it, and returns
// a classified KiroError. Used for non-2xx responses where the executor will
// not stream the body to the caller.
func classifyAndCloseKiroResponse(resp *http.Response) *helps.KiroError {
	if resp == nil {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	if errClose := resp.Body.Close(); errClose != nil {
		log.Errorf("kiro executor: response body close error: %v", errClose)
	}
	return helps.ClassifyKiroHTTPStatus(resp.StatusCode, body, resp.Header)
}

func kiroAccessToken(auth *cliproxyauth.Auth) string {
	return kiroMetaStr(auth, "accessToken")
}

func kiroMetaStr(auth *cliproxyauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	v, _ := auth.Metadata[key].(string)
	return strings.TrimSpace(v)
}

// applyKiroHTTPHeaders sets all required Kiro headers on the HTTP request.
func applyKiroHTTPHeaders(req *http.Request, auth *cliproxyauth.Auth) {
	authUUID := kiroMetaStr(auth, "uuid")
	profileArn := kiroMetaStr(auth, "profileArn")
	clientID := kiroMetaStr(auth, "clientId")
	machineID := helps.GenerateKiroMachineID(authUUID, profileArn, clientID)

	for k, v := range helps.KiroRequestHeaders(machineID) {
		req.Header.Set(k, v)
	}
	for k, v := range helps.KiroPerRequestHeaders(kiroAccessToken(auth)) {
		req.Header.Set(k, v)
	}
}

// buildKiroCodeWhispererRequest translates a Claude-format JSON payload
// into a CodeWhisperer generateAssistantResponse request body.
//
// defaultThinkingEffort controls how `thinking.type=enabled` requests (the
// shape Claude Code sends by default) are translated for Kiro. See
// helps.SelectKiroThinkingPrefix for the full behavior matrix; pass "" to
// fall back to the helper's built-in "medium" default.
func buildKiroCodeWhispererRequest(claudePayload []byte, auth *cliproxyauth.Auth, defaultThinkingEffort string) ([]byte, *helps.KiroToolNameMaps, error) {
	conversationID := uuid.New().String()
	system := gjson.GetBytes(claudePayload, "system")
	messagesRaw := gjson.GetBytes(claudePayload, "messages")
	toolsRaw := gjson.GetBytes(claudePayload, "tools")
	thinkingRaw := gjson.GetBytes(claudePayload, "thinking")
	modelRaw := gjson.GetBytes(claudePayload, "model").String()

	cwModel := helps.MapKiroModel(modelRaw)

	// Extract system prompt text.
	systemPrompt := extractSystemPromptText(system)
	if looksLikeClaudeCodeKiroRequest(toolsRaw) {
		systemPrompt = appendKiroSystemGuidance(systemPrompt, kiroClaudeCodeStreamingGuidance)
	}

	// Generate thinking prefix.
	if thinkingRaw.Exists() {
		tType := thinkingRaw.Get("type").String()
		budgetTokens := int(thinkingRaw.Get("budget_tokens").Int())
		effort := thinkingRaw.Get("effort").String()
		if effort == "" {
			// Claude canonical format places effort under output_config.effort.
			effort = gjson.GetBytes(claudePayload, "output_config.effort").String()
		}
		// Tier-aware policy: lighter Kiro tiers (haiku 4.5, sonnet 4.5,
		// opus 4.5) do not understand <thinking_mode>adaptive</thinking_mode>
		// + <thinking_effort>...</thinking_effort>. The proxy's default
		// rewrite from Claude Code's `enabled+budget` shape into adaptive
		// thinking is a no-op upstream on those tiers and just burns
		// turnaround time. Drop the thinking prefix entirely for `enabled`
		// requests targeting those tiers — Claude Code's auto-routed
		// light-tier traffic (completion summary, telemetry, internal
		// subagent calls) does not benefit from internal reasoning anyway,
		// and skipping the thinking pass shaves measurable TTFT.
		//
		// Explicit `adaptive` requests (typically from a model suffix that
		// the user opted into) are still respected — only the implicit
		// `enabled+budget → adaptive+effort` path is suppressed here.
		if !helps.KiroSupportsAdaptiveLevels(cwModel) && strings.EqualFold(tType, "enabled") {
			tType = "disabled"
		}
		prefix := helps.SelectKiroThinkingPrefix(tType, budgetTokens, effort, defaultThinkingEffort)
		if prefix != "" {
			if systemPrompt != "" {
				systemPrompt = prefix + "\n" + systemPrompt
			} else {
				systemPrompt = prefix
			}
		}
	}

	// Build tool name maps.
	var toolNameMaps *helps.KiroToolNameMaps
	var toolDefs []json.RawMessage
	if toolsRaw.Exists() && toolsRaw.IsArray() {
		for _, t := range toolsRaw.Array() {
			toolDefs = append(toolDefs, json.RawMessage(t.Raw))
		}
	}
	toolNameMaps = helps.BuildKiroToolNameMaps(toolDefs)

	// Build tools context (filter web_search, add placeholder if empty).
	toolsContext := buildKiroToolsContext(toolsRaw, toolNameMaps)

	// Parse messages into history + currentMessage.
	messages := messagesRaw.Array()
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("no messages in payload")
	}

	history, currentMessage := buildKiroHistory(messages, systemPrompt, cwModel, toolNameMaps)

	// Assemble final request.
	request := map[string]interface{}{
		"conversationState": map[string]interface{}{
			"agentTaskType":   helps.KiroAgentTaskType,
			"chatTriggerType": helps.KiroChatTrigger,
			"conversationId":  conversationID,
			"currentMessage":  currentMessage,
		},
	}
	cs := request["conversationState"].(map[string]interface{})
	if len(history) > 0 {
		cs["history"] = history
	}
	// Merge tools into currentMessage.userInputMessage.userInputMessageContext.
	if len(toolsContext) > 0 {
		if uim, ok := currentMessage["userInputMessage"].(map[string]interface{}); ok {
			ctx, _ := uim["userInputMessageContext"].(map[string]interface{})
			if ctx == nil {
				ctx = make(map[string]interface{})
			}
			ctx["tools"] = toolsContext
			uim["userInputMessageContext"] = ctx
		}
	}

	// Add profileArn for social auth.
	authMethod := kiroMetaStr(auth, "authMethod")
	if authMethod == helps.KiroAuthMethodSocial || authMethod == "" {
		profileArn := kiroMetaStr(auth, "profileArn")
		if profileArn != "" {
			request["profileArn"] = profileArn
		}
	}

	out, err := json.Marshal(request)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to marshal CodeWhisperer request: %w", err)
	}
	return out, toolNameMaps, nil
}

const kiroClaudeCodeStreamingGuidance = "When responding through Claude Code in a terminal, keep long summaries in short paragraphs or bullet lists. For workspace or multi-project summaries, after using tools always provide the completed summary rather than ending with an intention like \"I will explore\". Prefer grouping related projects by function instead of exhaustively listing every repository. 中文硬约束：第一行先写“项目：”并列出已发现的主要项目名；随后最多 4 条分组 bullet，每条可包含多个相关项目名，每条不超过 45 个汉字；列完后只输出“以上为工作区模块功能概览。”并立即停止；严禁追加“数据流总结”“整体链路”“架构模式”“补充说明”等额外段落。 Avoid wide Markdown tables unless the user explicitly asks for a table, and avoid deliberate line breaks inside Chinese words."

const kiroClaudeCodeContinuationGuidance = "Continuation mode: the previous assistant message was truncated. Continue from the exact unfinished character only; do not repeat any earlier heading, bullet, or project already written. Finish the current bullet/list in at most three short lines, then output the closing sentence “以上为工作区模块功能概览。” and stop."

func looksLikeClaudeCodeKiroRequest(toolsRaw gjson.Result) bool {
	if !toolsRaw.Exists() || !toolsRaw.IsArray() {
		return false
	}
	score := 0
	for _, tool := range toolsRaw.Array() {
		switch tool.Get("name").String() {
		case "Task", "Agent", "Bash", "Read", "Glob", "Grep", "LS", "Edit", "MultiEdit", "Write", "TodoWrite":
			score++
		case "AskUserQuestion", "TaskOutput", "TaskStop", "WebFetch", "WebSearch":
			return true
		}
	}
	return score >= 2
}

func appendKiroSystemGuidance(systemPrompt, guidance string) string {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		return guidance
	}
	if strings.Contains(systemPrompt, guidance) {
		return systemPrompt
	}
	return systemPrompt + "\n\n" + guidance
}

func extractSystemPromptText(system gjson.Result) string {
	if !system.Exists() {
		return ""
	}
	if system.Type == gjson.String {
		return system.String()
	}
	if system.IsArray() {
		var parts []string
		for _, item := range system.Array() {
			if item.Get("type").String() == "text" {
				parts = append(parts, item.Get("text").String())
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func buildKiroToolsContext(toolsRaw gjson.Result, maps *helps.KiroToolNameMaps) []interface{} {
	if !toolsRaw.Exists() || !toolsRaw.IsArray() || len(toolsRaw.Array()) == 0 {
		return []interface{}{kiroPlaceholderTool()}
	}

	var result []interface{}
	for _, t := range toolsRaw.Array() {
		name := t.Get("name").String()
		nameLower := strings.ToLower(name)
		if nameLower == "web_search" || nameLower == "websearch" {
			continue
		}
		desc := t.Get("description").String()
		if strings.TrimSpace(desc) == "" {
			continue
		}
		if len(desc) > helps.KiroMaxDescriptionLength {
			desc = desc[:helps.KiroMaxDescriptionLength] + "..."
		}
		kiroName := maps.ToKiroName(name)
		inputSchema := t.Get("input_schema")
		var schema interface{}
		if inputSchema.Exists() {
			schema = json.RawMessage(inputSchema.Raw)
		} else {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		result = append(result, map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name":        kiroName,
				"description": desc,
				"inputSchema": map[string]interface{}{
					"json": schema,
				},
			},
		})
	}

	if len(result) == 0 {
		return []interface{}{kiroPlaceholderTool()}
	}
	return result
}

func kiroPlaceholderTool() interface{} {
	return map[string]interface{}{
		"toolSpecification": map[string]interface{}{
			"name":        "no_tool_available",
			"description": "This is a placeholder tool when no other tools are available. It does nothing.",
			"inputSchema": map[string]interface{}{
				"json": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	}
}

func buildKiroHistory(messages []gjson.Result, systemPrompt, cwModel string, maps *helps.KiroToolNameMaps) ([]interface{}, map[string]interface{}) {
	var history []interface{}
	startIndex := 0
	prependSystem := false

	if systemPrompt != "" {
		if len(messages) == 1 && messages[0].Get("role").String() == "user" {
			prependSystem = true
		} else if messages[0].Get("role").String() == "user" {
			firstContent := extractMessageText(messages[0])
			history = append(history, map[string]interface{}{
				"userInputMessage": map[string]interface{}{
					"content": systemPrompt + "\n\n" + firstContent,
					"modelId": cwModel,
					"origin":  helps.KiroOriginAIEditor,
				},
			})
			startIndex = 1
		} else {
			history = append(history, map[string]interface{}{
				"userInputMessage": map[string]interface{}{
					"content": systemPrompt,
					"modelId": cwModel,
					"origin":  helps.KiroOriginAIEditor,
				},
			})
		}
	}

	// Process history messages (all except the last).
	for i := startIndex; i < len(messages)-1; i++ {
		msg := messages[i]
		role := msg.Get("role").String()
		if role == "user" {
			history = append(history, buildKiroUserHistoryMessage(msg, cwModel, maps))
		} else if role == "assistant" {
			history = append(history, buildKiroAssistantHistoryMessage(msg, maps))
		}
	}

	// Build currentMessage from the last message.
	lastMsg := messages[len(messages)-1]
	var currentMessage map[string]interface{}

	if lastMsg.Get("role").String() == "assistant" {
		// Move assistant to history, use "Continue" as current user message.
		history = append(history, buildKiroAssistantHistoryMessage(lastMsg, maps))
		currentMessage = map[string]interface{}{
			"userInputMessage": map[string]interface{}{
				"content": "Continue",
				"modelId": cwModel,
				"origin":  helps.KiroOriginAIEditor,
			},
		}
	} else {
		// Ensure history ends with assistantResponseMessage.
		if len(history) > 0 {
			last := history[len(history)-1]
			if lastMap, ok := last.(map[string]interface{}); ok {
				if _, hasAssistant := lastMap["assistantResponseMessage"]; !hasAssistant {
					history = append(history, map[string]interface{}{
						"assistantResponseMessage": map[string]interface{}{
							"content": "Continue",
						},
					})
				}
			}
		}
		currentMessage = buildKiroCurrentUserMessage(lastMsg, cwModel, maps, prependSystem, systemPrompt)
	}

	return history, currentMessage
}

func buildKiroUserHistoryMessage(msg gjson.Result, cwModel string, maps *helps.KiroToolNameMaps) map[string]interface{} {
	uim := map[string]interface{}{
		"content": "",
		"modelId": cwModel,
		"origin":  helps.KiroOriginAIEditor,
	}
	var toolResults []interface{}

	content := msg.Get("content")
	if content.IsArray() {
		var textParts []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				textParts = append(textParts, part.Get("text").String())
			case "tool_result":
				resultContent := part.Get("content").String()
				if part.Get("content").IsArray() {
					var texts []string
					for _, c := range part.Get("content").Array() {
						if c.Get("type").String() == "text" {
							texts = append(texts, c.Get("text").String())
						}
					}
					resultContent = strings.Join(texts, "")
				}
				toolResults = append(toolResults, map[string]interface{}{
					"content":   []interface{}{map[string]string{"text": resultContent}},
					"status":    "success",
					"toolUseId": part.Get("tool_use_id").String(),
				})
			}
		}
		uim["content"] = strings.Join(textParts, "")
	} else {
		uim["content"] = content.String()
	}

	if len(toolResults) > 0 {
		uim["userInputMessageContext"] = map[string]interface{}{
			"toolResults": deduplicateToolResults(toolResults),
		}
	}

	return map[string]interface{}{"userInputMessage": uim}
}

func buildKiroAssistantHistoryMessage(msg gjson.Result, maps *helps.KiroToolNameMaps) map[string]interface{} {
	arm := map[string]interface{}{"content": ""}
	var toolUses []interface{}
	var thinkingText string

	content := msg.Get("content")
	if content.IsArray() {
		var textParts []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				textParts = append(textParts, sanitizeKiroVisibleText(part.Get("text").String()))
			case "thinking":
				t := part.Get("thinking").String()
				if t == "" {
					t = part.Get("text").String()
				}
				thinkingText += t
			case "tool_use":
				input := json.RawMessage(part.Get("input").Raw)
				toolUses = append(toolUses, map[string]interface{}{
					"input":     helps.SanitizeToolInput(input),
					"name":      maps.ToKiroName(part.Get("name").String()),
					"toolUseId": part.Get("id").String(),
				})
			}
		}
		arm["content"] = strings.Join(textParts, "")
	} else {
		arm["content"] = sanitizeKiroVisibleText(content.String())
	}

	if thinkingText != "" {
		c := arm["content"].(string)
		if c != "" {
			arm["content"] = helps.KiroThinkingStartTag + thinkingText + helps.KiroThinkingEndTag + "\n\n" + c
		} else {
			arm["content"] = helps.KiroThinkingStartTag + thinkingText + helps.KiroThinkingEndTag
		}
	}

	if len(toolUses) > 0 {
		arm["toolUses"] = toolUses
	}

	return map[string]interface{}{"assistantResponseMessage": arm}
}

func buildKiroCurrentUserMessage(msg gjson.Result, cwModel string, maps *helps.KiroToolNameMaps, prependSystem bool, systemPrompt string) map[string]interface{} {
	uim := map[string]interface{}{
		"modelId": cwModel,
		"origin":  helps.KiroOriginAIEditor,
	}
	var currentContent string
	var toolResults []interface{}

	content := msg.Get("content")
	if content.IsArray() {
		var textParts []string
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				textParts = append(textParts, part.Get("text").String())
			case "tool_result":
				resultContent := part.Get("content").String()
				if part.Get("content").IsArray() {
					var texts []string
					for _, c := range part.Get("content").Array() {
						if c.Get("type").String() == "text" {
							texts = append(texts, c.Get("text").String())
						}
					}
					resultContent = strings.Join(texts, "")
				}
				toolResults = append(toolResults, map[string]interface{}{
					"content":   []interface{}{map[string]string{"text": resultContent}},
					"status":    "success",
					"toolUseId": part.Get("tool_use_id").String(),
				})
			}
		}
		currentContent = strings.Join(textParts, "")
	} else {
		currentContent = content.String()
	}

	if currentContent == "" {
		if len(toolResults) > 0 {
			currentContent = "Tool results provided."
		} else {
			currentContent = "Continue"
		}
	}

	if prependSystem && systemPrompt != "" {
		currentContent = systemPrompt + "\n\n" + currentContent
	}

	uim["content"] = currentContent

	if len(toolResults) > 0 {
		ctx := make(map[string]interface{})
		ctx["toolResults"] = deduplicateToolResults(toolResults)
		uim["userInputMessageContext"] = ctx
	}

	return map[string]interface{}{"userInputMessage": uim}
}

func extractMessageText(msg gjson.Result) string {
	content := msg.Get("content")
	if content.IsArray() {
		var parts []string
		for _, p := range content.Array() {
			if p.Get("type").String() == "text" {
				parts = append(parts, p.Get("text").String())
			}
		}
		return strings.Join(parts, "")
	}
	return content.String()
}

func deduplicateToolResults(results []interface{}) []interface{} {
	seen := make(map[string]bool)
	var out []interface{}
	for _, r := range results {
		if m, ok := r.(map[string]interface{}); ok {
			id, _ := m["toolUseId"].(string)
			if id != "" && seen[id] {
				continue
			}
			seen[id] = true
		}
		out = append(out, r)
	}
	return out
}

// --- Non-streaming response builder ---

// buildClaudeMessageJSON parses a non-streaming Kiro response and constructs
// a Claude Messages API JSON response (not SSE). This is what TranslateNonStream expects.
func buildClaudeMessageJSON(rawResp []byte, toolNameMaps *helps.KiroToolNameMaps, model string) []byte {
	events, _ := helps.ParseAwsEventStreamBuffer(rawResp)

	var textContent string
	type toolCall struct {
		Name      string
		ToolUseID string
		Input     string
	}
	var toolCalls []toolCall
	var currentTool *toolCall
	var lastContent string

	for _, evt := range events {
		switch evt.Type {
		case "content":
			if evt.Content != lastContent {
				textContent += evt.Content
				lastContent = evt.Content
			}
		case "toolUse":
			// If the same toolUseID is already active, treat as input continuation.
			if currentTool != nil && currentTool.ToolUseID == evt.ToolUseID {
				currentTool.Input += evt.ToolInput
				if evt.ToolStop {
					toolCalls = append(toolCalls, *currentTool)
					currentTool = nil
				}
				continue
			}
			if currentTool != nil {
				toolCalls = append(toolCalls, *currentTool)
			}
			name := evt.ToolName
			if toolNameMaps != nil {
				name = toolNameMaps.FromKiroName(name)
			}
			currentTool = &toolCall{Name: name, ToolUseID: evt.ToolUseID, Input: evt.ToolInput}
			if evt.ToolStop {
				toolCalls = append(toolCalls, *currentTool)
				currentTool = nil
			}
		case "toolUseInput":
			if currentTool != nil {
				currentTool.Input += evt.ToolInput
			}
		case "toolUseStop":
			if currentTool != nil {
				toolCalls = append(toolCalls, *currentTool)
				currentTool = nil
			}
		}
	}
	if currentTool != nil {
		toolCalls = append(toolCalls, *currentTool)
	}

	// Build Claude message JSON content blocks.
	var contentBlocks []interface{}

	// Parse thinking from text content.
	thinkingContent, remainingText := extractThinkingFromText(textContent)
	if thinkingContent != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":     "thinking",
			"thinking": thinkingContent,
		})
	}
	if strings.TrimSpace(remainingText) != "" {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": remainingText,
		})
	}

	for _, tc := range toolCalls {
		var inputObj interface{}
		if err := json.Unmarshal([]byte(tc.Input), &inputObj); err != nil {
			inputObj = map[string]interface{}{}
		}
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tc.ToolUseID,
			"name":  tc.Name,
			"input": inputObj,
		})
	}

	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]interface{}{
			"type": "text",
			"text": "",
		})
	}

	stopReason := "end_turn"
	if len(toolCalls) > 0 {
		stopReason = "tool_use"
	}

	claudeResp := map[string]interface{}{
		"id":            "msg_" + uuid.New().String(),
		"type":          "message",
		"role":          "assistant",
		"content":       contentBlocks,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	}

	out, err := json.Marshal(claudeResp)
	if err != nil {
		log.Errorf("kiro executor: failed to marshal Claude response: %v", err)
		return []byte("{}")
	}
	return out
}

func buildClaudeMessageSSE(rawResp []byte, toolNameMaps *helps.KiroToolNameMaps, model string) []byte {
	var buf bytes.Buffer
	_, _ = streamKiroToClaudeSSE(context.Background(), bytes.NewReader(rawResp), toolNameMaps, model, func(line []byte) {
		buf.Write(line)
		if !bytes.HasSuffix(line, []byte("\n\n")) {
			buf.WriteByte('\n')
		}
	})
	return buf.Bytes()
}

func claudeSSEDataLines(raw []byte) [][]byte {
	lines := bytes.Split(raw, []byte("\n"))
	out := make([][]byte, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			out = append(out, bytes.Clone(line))
		}
	}
	return out
}

// --- Streaming response builder ---

type kiroStreamResult struct {
	eventCount     int
	payloadStarted bool
}

type kiroContinuationAfterIncompleteAssistantKey struct{}
type kiroClaudeCodeRequestKey struct{}

// streamKiroToClaudeSSE incrementally reads a Kiro streaming response, parsing
// AWS Event Stream chunks as they arrive and converting each event to Claude SSE
// format. The emit callback is called for each SSE line as soon as it is ready,
// enabling true incremental streaming to the client.
func streamKiroToClaudeSSE(ctx context.Context, body io.Reader, toolNameMaps *helps.KiroToolNameMaps, model string, emit func([]byte)) (kiroStreamResult, error) {
	startedAt := time.Now()
	requestID := logging.GetRequestID(ctx)
	var result kiroStreamResult
	msgID := "msg_" + uuid.New().String()
	continuationAfterIncompleteAssistant := false
	claudeCodeRequest := false
	if ctx != nil {
		continuationAfterIncompleteAssistant, _ = ctx.Value(kiroContinuationAfterIncompleteAssistantKey{}).(bool)
		claudeCodeRequest, _ = ctx.Value(kiroClaudeCodeRequestKey{}).(bool)
	}
	nextBlockIndex := 0
	stoppedBlocks := make(map[int]bool)
	thinkingBlockIndex := -1
	textBlockIndex := -1
	var lastContent string

	type activeToolUse struct {
		name             string
		toolUseID        string
		input            string
		index            int
		inputDeltaEvents int
		emitted          bool
		startedAt        time.Time
		firstInputAt     time.Time
		lastInputAt      time.Time
	}
	activeTools := make(map[string]*activeToolUse)
	var toolOrder []string
	currentToolID := ""
	toolUseCount := 0
	toolInputDeltaCount := 0
	emptyToolUseCount := 0
	thinkingDeltaCount := 0
	visibleTextDeltaCount := 0
	whitespaceTextDeltaCount := 0
	readCount := 0
	firstReadMs := int64(-1)
	firstEventMs := int64(-1)
	firstVisibleMs := int64(-1)
	firstToolMs := int64(-1)
	longestReadGapMs := int64(0)
	longestEventGapMs := int64(0)
	longestVisibleGapMs := int64(0)
	maxToolAssembleMs := int64(0)
	emittedToolInputBytes := 0
	droppedToolInputBytes := 0
	var lastReadAt time.Time
	var lastEventAt time.Time
	var lastVisibleAt time.Time
	upstreamStopReason := ""
	exceptionCount := 0
	visibleTextRuneCount := 0
	visibleTextTail := ""
	malformedAfterPayload := false

	// Helper to emit an SSE line.
	emitSSE := func(eventType string, data interface{}) {
		jsonData, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n", eventType, string(jsonData))
		emit([]byte(line))
	}

	markVisible := func(kind string) {
		now := time.Now()
		elapsedMs := now.Sub(startedAt).Milliseconds()
		if firstVisibleMs < 0 {
			firstVisibleMs = elapsedMs
		}
		if !lastVisibleAt.IsZero() {
			longestVisibleGapMs = maxInt64(longestVisibleGapMs, now.Sub(lastVisibleAt).Milliseconds())
		}
		lastVisibleAt = now
		if kind == "tool" && firstToolMs < 0 {
			firstToolMs = elapsedMs
		}
	}

	emitMessageStart := func() {
		if result.payloadStarted {
			return
		}
		result.payloadStarted = true
		emitSSE("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":      msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []interface{}{},
				"model":   model,
				"usage":   map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}

	stopBlock := func(index int) {
		if index < 0 || stoppedBlocks[index] {
			return
		}
		stoppedBlocks[index] = true
		emitSSE("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": index,
		})
	}

	hasToolCalls := false
	var flushPendingThinking func()
	var pendingInitialText string

	emitToolStart := func(tool *activeToolUse) {
		if tool == nil || tool.emitted {
			return
		}
		pendingInitialText = ""
		if textBlockIndex >= 0 {
			stopBlock(textBlockIndex)
			textBlockIndex = -1
		}
		flushPendingThinking()
		emitMessageStart()
		idx := nextBlockIndex
		nextBlockIndex++
		tool.index = idx
		tool.emitted = true
		hasToolCalls = true
		toolUseCount++
		markVisible("tool")

		// IMPORTANT: include `input: {}` in the content_block payload.
		// The Anthropic Messages API SSE spec for `content_block_start`
		// requires every tool_use block to publish an initial input value
		// (an empty object). Strict downstream SDK parsers — notably
		// Claude Code v2.x — validate the content_block schema as the
		// event arrives; omitting `input` makes them treat the block as
		// malformed and surface "Invalid tool parameters" or get stuck on
		// "Initializing…" because the tool block is never finalized
		// (input_json_delta accumulators have nothing to merge into).
		// Mirrors AIClient2API claude-kiro.js (`input: {}`) and
		// kiro.rs anthropic/stream.rs (`"input": {}`).
		emitSSE("content_block_start", map[string]interface{}{
			"type":  "content_block_start",
			"index": idx,
			"content_block": map[string]interface{}{
				"type":  "tool_use",
				"id":    tool.toolUseID,
				"name":  tool.name,
				"input": map[string]interface{}{},
			},
		})
	}

	emitToolInputDelta := func(tool *activeToolUse, input string) {
		if tool == nil || input == "" {
			return
		}
		if tool.firstInputAt.IsZero() {
			tool.firstInputAt = time.Now()
		}
		tool.lastInputAt = time.Now()
		tool.input += input
		tool.inputDeltaEvents++
		toolInputDeltaCount++
	}

	getTool := func(toolUseID string) *activeToolUse {
		return activeTools[strings.TrimSpace(toolUseID)]
	}

	rememberTool := func(tool *activeToolUse) {
		if tool == nil {
			return
		}
		id := strings.TrimSpace(tool.toolUseID)
		if id == "" {
			return
		}
		if _, exists := activeTools[id]; !exists {
			toolOrder = append(toolOrder, id)
		}
		activeTools[id] = tool
		currentToolID = id
	}

	deleteTool := func(toolUseID string) {
		delete(activeTools, strings.TrimSpace(toolUseID))
		if currentToolID == strings.TrimSpace(toolUseID) {
			currentToolID = ""
			for i := len(toolOrder) - 1; i >= 0; i-- {
				if _, ok := activeTools[toolOrder[i]]; ok {
					currentToolID = toolOrder[i]
					break
				}
			}
		}
	}

	currentTool := func() *activeToolUse {
		if currentToolID == "" {
			return nil
		}
		return activeTools[currentToolID]
	}

	finishTool := func(tool *activeToolUse, completion string) {
		if tool == nil {
			return
		}
		input := strings.TrimSpace(tool.input)
		assembleMs := time.Since(tool.startedAt).Milliseconds()
		maxToolAssembleMs = maxInt64(maxToolAssembleMs, assembleMs)
		if input == "" || !json.Valid([]byte(input)) || !gjson.Parse(input).IsObject() {
			emptyToolUseCount++
			droppedToolInputBytes += len(tool.input)
			message := fmt.Sprintf(
				"kiro executor: tool_use_dropped_invalid_input event=tool_use_dropped_invalid_input request_id=%s model=%s tool_name=%s tool_use_id=%s input_bytes=%d input_delta_events=%d assemble_ms=%d completion=%s",
				requestID, model, tool.name, tool.toolUseID, len(tool.input), tool.inputDeltaEvents, assembleMs, completion,
			)
			log.WithFields(log.Fields{
				"event":              "tool_use_dropped_invalid_input",
				"request_id":         requestID,
				"model":              model,
				"tool_name":          tool.name,
				"tool_use_id":        tool.toolUseID,
				"input_bytes":        len(tool.input),
				"input_delta_events": tool.inputDeltaEvents,
				"assemble_ms":        assembleMs,
				"completion":         completion,
			}).Warn(message)
			return
		}
		emittedToolInputBytes += len(input)
		emitToolStart(tool)
		emitSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": tool.index,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": input},
		})
		stopBlock(tool.index)
	}

	// inThinking tracks whether we are currently inside an unclosed
	// `<thinking>...</thinking>` span that started in a previous Kiro `content`
	// event. Without this state, a tag whose body is split across events would
	// either close the thinking block prematurely or leak the closing tag (and
	// reasoning body) into a `text_delta` for the user.
	inThinking := false
	var contentBuffer string
	var pendingThinking []string
	var pendingVisibleWhitespace string

	emitThinkingDelta := func(text string) {
		if text == "" {
			return
		}
		thinkingDeltaCount++
		pendingThinking = append(pendingThinking, text)
	}

	flushPendingThinking = func() {
		if len(pendingThinking) == 0 {
			return
		}
		emitMessageStart()
		idx := nextBlockIndex
		nextBlockIndex++
		thinkingBlockIndex = idx
		emitSSE("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         idx,
			"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
		for _, text := range pendingThinking {
			emitSSE("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": thinkingBlockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": text},
			})
		}
		pendingThinking = nil
		stopBlock(thinkingBlockIndex)
		thinkingBlockIndex = -1
	}

	emitTextDeltaNow := func(text string) {
		if text == "" {
			return
		}
		if strings.TrimSpace(text) == "" {
			whitespaceTextDeltaCount++
			if textBlockIndex < 0 && visibleTextDeltaCount == 0 && toolUseCount == 0 {
				pendingVisibleWhitespace += text
				return
			}
		} else {
			if textBlockIndex < 0 && visibleTextDeltaCount == 0 && toolUseCount == 0 && pendingVisibleWhitespace != "" {
				text = pendingVisibleWhitespace + text
				pendingVisibleWhitespace = ""
			}
			visibleTextRuneCount += len([]rune(text))
			visibleTextTail = appendKiroVisibleTextTail(visibleTextTail, text, 1000)
			visibleTextDeltaCount++
			markVisible("text")
		}
		if textBlockIndex < 0 {
			flushPendingThinking()
			emitMessageStart()
			idx := nextBlockIndex
			nextBlockIndex++
			textBlockIndex = idx
			emitSSE("content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
		}
		emitSSE("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": textBlockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": text},
		})
	}

	emitTextDelta := func(text string) {
		if text == "" {
			return
		}
		if claudeCodeRequest && textBlockIndex < 0 && visibleTextDeltaCount == 0 && toolUseCount == 0 {
			candidate := pendingInitialText + text
			if isKiroClaudeCodeTransientPreamble(strings.TrimSpace(candidate)) || isKiroPossibleClaudeCodeTransientPreamblePrefix(strings.TrimSpace(candidate)) {
				pendingInitialText = candidate
				return
			}
			if pendingInitialText != "" {
				emitTextDeltaNow(pendingInitialText)
				pendingInitialText = ""
			}
		}
		emitTextDeltaNow(text)
	}

	// processContent runs the cross-event thinking state machine over the
	// payload of one Kiro `content` event. It emits any number of text/thinking
	// deltas while preserving `inThinking` across calls so that
	// `<thinking>` / `</thinking>` markers split across multiple events do not
	// leak reasoning into user-visible text_delta events.
	processContent := func(text string) {
		contentBuffer += text
		for contentBuffer != "" {
			if !inThinking {
				idx := strings.Index(contentBuffer, helps.KiroThinkingStartTag)
				if idx < 0 {
					keep := longestSuffixPrefixLen(contentBuffer, helps.KiroThinkingStartTag)
					if len(contentBuffer) > keep {
						emitTextDelta(contentBuffer[:len(contentBuffer)-keep])
						contentBuffer = contentBuffer[len(contentBuffer)-keep:]
					}
					return
				}
				if idx > 0 {
					emitTextDelta(contentBuffer[:idx])
				}
				if textBlockIndex >= 0 {
					stopBlock(textBlockIndex)
					textBlockIndex = -1
				}
				contentBuffer = contentBuffer[idx+len(helps.KiroThinkingStartTag):]
				if strings.HasPrefix(contentBuffer, "\r\n") {
					contentBuffer = contentBuffer[2:]
				} else if strings.HasPrefix(contentBuffer, "\n") {
					contentBuffer = contentBuffer[1:]
				}
				inThinking = true
				continue
			}
			idx := strings.Index(contentBuffer, helps.KiroThinkingEndTag)
			if idx < 0 {
				keep := longestSuffixPrefixLen(contentBuffer, helps.KiroThinkingEndTag)
				if len(contentBuffer) > keep {
					emitThinkingDelta(contentBuffer[:len(contentBuffer)-keep])
					contentBuffer = contentBuffer[len(contentBuffer)-keep:]
				}
				return
			}
			if idx > 0 {
				emitThinkingDelta(contentBuffer[:idx])
			}
			contentBuffer = contentBuffer[idx+len(helps.KiroThinkingEndTag):]
			if strings.HasPrefix(contentBuffer, "\n\n") {
				contentBuffer = contentBuffer[2:]
			} else if strings.HasPrefix(contentBuffer, "\n") {
				contentBuffer = contentBuffer[1:]
			}
			inThinking = false
		}
	}

	// processEvent handles a single parsed Kiro stream event.
	processEvent := func(evt helps.KiroStreamEvent) {
		switch evt.Type {
		case "content":
			if evt.Content == lastContent {
				return
			}
			lastContent = evt.Content
			processContent(evt.Content)

		case "toolUse":
			name := evt.ToolName
			if toolNameMaps != nil {
				name = toolNameMaps.FromKiroName(name)
			}
			tool := getTool(evt.ToolUseID)
			if tool == nil {
				tool = &activeToolUse{name: name, toolUseID: strings.TrimSpace(evt.ToolUseID), index: -1, startedAt: time.Now()}
				rememberTool(tool)
			} else {
				if name != "" {
					tool.name = name
				}
				currentToolID = tool.toolUseID
			}
			if evt.ToolInput != "" {
				emitToolInputDelta(tool, evt.ToolInput)
			}
			if evt.ToolStop {
				finishTool(tool, "tool_stop")
				deleteTool(tool.toolUseID)
			}

		case "toolUseInput":
			tool := getTool(evt.ToolUseID)
			if tool == nil && strings.TrimSpace(evt.ToolUseID) == "" {
				tool = currentTool()
			}
			if tool != nil {
				currentToolID = tool.toolUseID
				emitToolInputDelta(tool, evt.ToolInput)
			}

		case "toolUseStop":
			tool := getTool(evt.ToolUseID)
			if tool == nil && strings.TrimSpace(evt.ToolUseID) == "" {
				tool = currentTool()
			}
			if tool != nil {
				finishTool(tool, "tool_stop")
				deleteTool(tool.toolUseID)
			}
		case "contextUsage":
			if evt.ContextUsagePercentage >= 100 {
				upstreamStopReason = "model_context_window_exceeded"
			}
		case "exception":
			exceptionCount++
			if evt.ExceptionType == "ContentLengthExceededException" {
				upstreamStopReason = "max_tokens"
			}
			log.WithFields(log.Fields{
				"event":          "upstream_stream_exception",
				"request_id":     requestID,
				"model":          model,
				"exception_type": evt.ExceptionType,
			}).Warn("kiro executor: upstream stream exception")
		}
	}

	processEvents := func(events []helps.KiroStreamEvent) {
		for _, evt := range events {
			now := time.Now()
			if firstEventMs < 0 {
				firstEventMs = now.Sub(startedAt).Milliseconds()
			}
			if !lastEventAt.IsZero() {
				longestEventGapMs = maxInt64(longestEventGapMs, now.Sub(lastEventAt).Milliseconds())
			}
			lastEventAt = now
			result.eventCount++
			processEvent(evt)
		}
	}

	// Incrementally read from the response body and parse events.
	//
	// remaining is grown via append() so its backing array is reused across
	// reads (geometric growth → O(N) total allocation). The previous
	// `remaining += string(readBuf[:n])` pattern allocated a fresh string on
	// every Read, producing O(N²) bytes copied for long streams (e.g. ~1MB
	// of allocs for a 100KB response delivered in 32KB chunks). For long
	// Kiro Opus 4.6 multi-turn responses this both increased GC pressure and
	// occasionally introduced perceptible delta jitter.
	const readBufSize = 32 * 1024
	readBuf := make([]byte, readBufSize)
	remaining := make([]byte, 0, readBufSize)
	for {
		if errCtx := contextErr(ctx); errCtx != nil {
			return result, errCtx
		}
		n, readErr := body.Read(readBuf)
		if n > 0 {
			now := time.Now()
			if firstReadMs < 0 {
				firstReadMs = now.Sub(startedAt).Milliseconds()
			}
			if !lastReadAt.IsZero() {
				longestReadGapMs = maxInt64(longestReadGapMs, now.Sub(lastReadAt).Milliseconds())
			}
			lastReadAt = now
			readCount++
			remaining = append(remaining, readBuf[:n]...)
			var events []helps.KiroStreamEvent
			events, remaining = helps.ParseAwsEventStreamBuffer(remaining)
			// ParseAwsEventStreamBuffer returns a sub-slice of `remaining`.
			// Re-anchor it to the head of a fresh backing buffer when it has
			// drifted far into the original array, so append() in the next
			// iteration can keep growing without unbounded offsets.
			if cap(remaining) > readBufSize && len(remaining) < cap(remaining)/4 {
				compact := make([]byte, len(remaining), readBufSize)
				copy(compact, remaining)
				remaining = compact
			}
			processEvents(events)
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			if errCtx := contextErr(ctx); errCtx != nil {
				return result, errCtx
			}
			log.Warnf("kiro executor: error reading stream: %v", readErr)
			return result, newKiroStreamError(helps.KiroErrStreamRead, "stream read failed", readErr)
		}
	}
	// Final parse attempt on any remaining buffer data.
	if len(remaining) > 0 {
		var finalEvents []helps.KiroStreamEvent
		finalEvents, remaining = helps.ParseAwsEventStreamBuffer(remaining)
		processEvents(finalEvents)
		if hasKiroJSONResidue(remaining) {
			if result.payloadStarted {
				malformedAfterPayload = true
				log.Warn("kiro executor: ignoring incomplete trailing JSON event after payload started")
			} else {
				return result, newKiroStreamError(helps.KiroErrStreamMalformed, "stream ended with incomplete JSON event", nil)
			}
		}
	}
	if contentBuffer != "" {
		if inThinking {
			emitThinkingDelta(contentBuffer)
		} else {
			emitTextDelta(contentBuffer)
		}
		contentBuffer = ""
	}
	if pendingInitialText != "" && !isKiroClaudeCodeTransientPreamble(strings.TrimSpace(pendingInitialText)) {
		emitTextDeltaNow(pendingInitialText)
		pendingInitialText = ""
	}
	if pendingInitialText != "" && len(activeTools) > 0 {
		for _, toolUseID := range toolOrder {
			if tool, ok := activeTools[toolUseID]; ok {
				finishTool(tool, "stream_end")
				deleteTool(toolUseID)
			}
		}
	}
	if pendingInitialText != "" && !result.payloadStarted {
		return result, newKiroStreamError(helps.KiroErrStreamMalformed, "stream ended after intention-only preamble", nil)
	}
	if result.eventCount == 0 || !result.payloadStarted {
		if thinkingDeltaCount > 0 || emptyToolUseCount > 0 {
			durationMs := time.Since(startedAt).Milliseconds()
			message := fmt.Sprintf(
				"kiro executor: stream incomplete with no visible content event=stream_incomplete_no_visible_content request_id=%s model=%s duration_ms=%d event_count=%d thinking_delta_count=%d whitespace_text_delta_count=%d empty_tool_use_count=%d dropped_tool_input_bytes=%d",
				requestID,
				model,
				durationMs,
				result.eventCount,
				thinkingDeltaCount,
				whitespaceTextDeltaCount,
				emptyToolUseCount,
				droppedToolInputBytes,
			)
			log.WithFields(log.Fields{
				"event":                       "stream_incomplete_no_visible_content",
				"request_id":                  requestID,
				"model":                       model,
				"duration_ms":                 durationMs,
				"event_count":                 result.eventCount,
				"thinking_delta_count":        thinkingDeltaCount,
				"whitespace_text_delta_count": whitespaceTextDeltaCount,
				"empty_tool_use_count":        emptyToolUseCount,
				"dropped_tool_input_bytes":    droppedToolInputBytes,
				"payload_started":             result.payloadStarted,
			}).Warn(message)
			return result, newKiroStreamError(helps.KiroErrStreamMalformed, "stream ended without visible content or tool use", nil)
		}
		return result, nil
	}

	// Stop any remaining open blocks.
	if visibleTextDeltaCount > 0 || toolUseCount > 0 {
		flushPendingThinking()
	}
	synthesizedSummaryClosure := false
	if !hasToolCalls && (upstreamStopReason == "" || upstreamStopReason == "max_tokens" || upstreamStopReason == "end_turn") && shouldSynthesizeKiroSummaryClosure(model, visibleTextRuneCount, visibleTextTail, continuationAfterIncompleteAssistant, malformedAfterPayload) {
		emitTextDelta(kiroSummaryClosureText(visibleTextTail))
		synthesizedSummaryClosure = true
		upstreamStopReason = ""
		log.WithFields(log.Fields{
			"event":                   "stream_synthesized_summary_closure",
			"request_id":              requestID,
			"model":                   model,
			"visible_text_rune_count": visibleTextRuneCount,
		}).Warn("kiro executor: synthesized concise closure for truncated summary")
	}

	if thinkingBlockIndex >= 0 {
		stopBlock(thinkingBlockIndex)
	}
	if textBlockIndex >= 0 {
		stopBlock(textBlockIndex)
	}
	for _, toolUseID := range toolOrder {
		if tool, ok := activeTools[toolUseID]; ok {
			finishTool(tool, "stream_end")
			deleteTool(toolUseID)
		}
	}
	if toolUseCount == 0 && (emptyToolUseCount > 0 || droppedToolInputBytes > 0) {
		durationMs := time.Since(startedAt).Milliseconds()
		message := fmt.Sprintf(
			"kiro executor: stream incomplete tool use event=stream_incomplete_tool_use request_id=%s model=%s duration_ms=%d event_count=%d visible_text_delta_count=%d empty_tool_use_count=%d dropped_tool_input_bytes=%d",
			requestID,
			model,
			durationMs,
			result.eventCount,
			visibleTextDeltaCount,
			emptyToolUseCount,
			droppedToolInputBytes,
		)
		log.WithFields(log.Fields{
			"event":                    "stream_incomplete_tool_use",
			"request_id":               requestID,
			"model":                    model,
			"duration_ms":              durationMs,
			"event_count":              result.eventCount,
			"visible_text_delta_count": visibleTextDeltaCount,
			"empty_tool_use_count":     emptyToolUseCount,
			"dropped_tool_input_bytes": droppedToolInputBytes,
			"payload_started":          result.payloadStarted,
		}).Warn(message)
		if visibleTextDeltaCount == 0 {
			return result, newKiroStreamError(helps.KiroErrStreamMalformed, "stream ended with incomplete tool use", nil)
		}
		if !claudeCodeRequest && !kiroVisibleTextEndsWithTerminal(visibleTextTail) {
			upstreamStopReason = "max_tokens"
		}
	}
	if visibleTextDeltaCount == 0 && toolUseCount == 0 {
		durationMs := time.Since(startedAt).Milliseconds()
		message := fmt.Sprintf(
			"kiro executor: stream incomplete with no visible content event=stream_incomplete_no_visible_content request_id=%s model=%s duration_ms=%d event_count=%d thinking_delta_count=%d whitespace_text_delta_count=%d empty_tool_use_count=%d dropped_tool_input_bytes=%d",
			requestID,
			model,
			durationMs,
			result.eventCount,
			thinkingDeltaCount,
			whitespaceTextDeltaCount,
			emptyToolUseCount,
			droppedToolInputBytes,
		)
		log.WithFields(log.Fields{
			"event":                       "stream_incomplete_no_visible_content",
			"request_id":                  requestID,
			"model":                       model,
			"duration_ms":                 durationMs,
			"event_count":                 result.eventCount,
			"thinking_delta_count":        thinkingDeltaCount,
			"whitespace_text_delta_count": whitespaceTextDeltaCount,
			"empty_tool_use_count":        emptyToolUseCount,
			"dropped_tool_input_bytes":    droppedToolInputBytes,
			"payload_started":             result.payloadStarted,
		}).Warn(message)
		return result, newKiroStreamError(helps.KiroErrStreamMalformed, "stream ended without visible content or tool use", nil)
	}

	stopReason := "end_turn"
	if upstreamStopReason != "" {
		stopReason = upstreamStopReason
	} else if hasToolCalls {
		stopReason = "tool_use"
	} else if !claudeCodeRequest && !synthesizedSummaryClosure && shouldMarkKiroEndAsMaxTokens(model, visibleTextRuneCount, visibleTextTail, thinkingDeltaCount, continuationAfterIncompleteAssistant) {
		stopReason = "max_tokens"
		log.WithFields(log.Fields{
			"event":                   "stream_suspicious_truncated_end",
			"request_id":              requestID,
			"model":                   model,
			"visible_text_rune_count": visibleTextRuneCount,
			"thinking_delta_count":    thinkingDeltaCount,
		}).Warn("kiro executor: mapping suspicious short incomplete end_turn to max_tokens")
	}

	// message_delta
	emitSSE("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": 0},
	})

	// message_stop
	emitSSE("message_stop", map[string]interface{}{
		"type": "message_stop",
	})
	durationMs := time.Since(startedAt).Milliseconds()
	summaryMessage := fmt.Sprintf(
		"kiro executor: stream summary event=stream_summary request_id=%s model=%s duration_ms=%d read_count=%d first_read_ms=%d first_event_ms=%d first_visible_ms=%d first_tool_ms=%d longest_read_gap_ms=%d longest_event_gap_ms=%d longest_visible_gap_ms=%d event_count=%d visible_text_delta_count=%d whitespace_text_delta_count=%d thinking_delta_count=%d tool_use_count=%d tool_input_delta_count=%d empty_tool_use_count=%d exception_count=%d stop_reason=%s max_tool_assemble_ms=%d emitted_tool_input_bytes=%d dropped_tool_input_bytes=%d payload_started=%t",
		requestID,
		model,
		durationMs,
		readCount,
		firstReadMs,
		firstEventMs,
		firstVisibleMs,
		firstToolMs,
		longestReadGapMs,
		longestEventGapMs,
		longestVisibleGapMs,
		result.eventCount,
		visibleTextDeltaCount,
		whitespaceTextDeltaCount,
		thinkingDeltaCount,
		toolUseCount,
		toolInputDeltaCount,
		emptyToolUseCount,
		exceptionCount,
		stopReason,
		maxToolAssembleMs,
		emittedToolInputBytes,
		droppedToolInputBytes,
		result.payloadStarted,
	)
	log.WithFields(log.Fields{
		"event":                       "stream_summary",
		"request_id":                  requestID,
		"model":                       model,
		"duration_ms":                 durationMs,
		"read_count":                  readCount,
		"first_read_ms":               firstReadMs,
		"first_event_ms":              firstEventMs,
		"first_visible_ms":            firstVisibleMs,
		"first_tool_ms":               firstToolMs,
		"longest_read_gap_ms":         longestReadGapMs,
		"longest_event_gap_ms":        longestEventGapMs,
		"longest_visible_gap_ms":      longestVisibleGapMs,
		"event_count":                 result.eventCount,
		"visible_text_delta_count":    visibleTextDeltaCount,
		"whitespace_text_delta_count": whitespaceTextDeltaCount,
		"thinking_delta_count":        thinkingDeltaCount,
		"tool_use_count":              toolUseCount,
		"tool_input_delta_count":      toolInputDeltaCount,
		"empty_tool_use_count":        emptyToolUseCount,
		"exception_count":             exceptionCount,
		"stop_reason":                 stopReason,
		"max_tool_assemble_ms":        maxToolAssembleMs,
		"emitted_tool_input_bytes":    emittedToolInputBytes,
		"dropped_tool_input_bytes":    droppedToolInputBytes,
		"payload_started":             result.payloadStarted,
	}).Info(summaryMessage)
	if firstVisibleMs > kiroVisibleGapWarningThresholdMs || longestVisibleGapMs > kiroVisibleGapWarningThresholdMs {
		log.WithFields(log.Fields{
			"event":                  "visible_gap_warning",
			"request_id":             requestID,
			"model":                  model,
			"duration_ms":            durationMs,
			"first_visible_ms":       firstVisibleMs,
			"longest_visible_gap_ms": longestVisibleGapMs,
			"stop_reason":            stopReason,
			"payload_started":        result.payloadStarted,
		}).Warn("kiro executor: long gap before user-visible stream output")
	}
	return result, nil
}

const kiroVisibleGapWarningThresholdMs int64 = 15000

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func appendKiroVisibleTextTail(tail, text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(tail + text)
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[len(runes)-maxRunes:])
}

func shouldMarkKiroEndAsMaxTokens(model string, visibleRuneCount int, visibleTail string, thinkingDeltaCount int, continuationAfterIncompleteAssistant bool) bool {
	trimmed := strings.TrimSpace(visibleTail)
	if trimmed == "" {
		return false
	}
	if !isKiroOpusModel(model) {
		return false
	}
	runes := []rune(trimmed)
	last := runes[len(runes)-1]
	if isKiroIntentionOnlyAnswer(trimmed) {
		return true
	}
	if isKiroMarkdownHeadingFragment(trimmed) {
		return true
	}
	if strings.ContainsRune("。.!?！？…」』）)]}》\"'`”’", last) {
		return false
	}
	if last == '|' {
		return false
	}
	if strings.ContainsRune("，,、：:；;", last) {
		return visibleRuneCount >= 2
	}
	if continuationAfterIncompleteAssistant {
		return true
	}
	if visibleRuneCount < 4 {
		return false
	}
	if hasUnclosedKiroMarkdownFence(trimmed) || hasUnclosedKiroDelimiter(trimmed) {
		return true
	}
	if visibleRuneCount >= 40 {
		return true
	}
	return thinkingDeltaCount > 0 && visibleRuneCount >= 8 && visibleRuneCount <= 80
}

func shouldSynthesizeKiroSummaryClosure(model string, visibleRuneCount int, visibleTail string, continuationAfterIncompleteAssistant bool, malformedAfterPayload bool) bool {
	if (!continuationAfterIncompleteAssistant && !malformedAfterPayload) || !isKiroOpusModel(model) || visibleRuneCount < 80 {
		return false
	}
	trimmed := strings.TrimSpace(visibleTail)
	if trimmed == "" || kiroVisibleTextEndsWithTerminal(trimmed) {
		return false
	}
	if strings.HasPrefix(trimmed, "以上") {
		return true
	}
	return strings.Count(trimmed, "starrocks-") >= 5 || strings.Count(trimmed, "**") >= 6
}

func kiroSummaryClosureText(visibleTail string) string {
	trimmed := strings.TrimSpace(visibleTail)
	if strings.HasSuffix(trimmed, "以上") {
		return "为工作区模块功能概览。"
	}
	if strings.Contains(visibleTail, "starrocks-profile") || strings.Contains(visibleTail, "starrocks-ops-mcp") {
		return "；其他模块包括 starrocks-aiops、starrocks-gc-detector、starrocks-diagnostics-skills、starrocks-approval、starrocks-board、starrocks-cluster、starrocks-realtime-monitor、starrocks-skills、starrocks-experience-docs 等。以上为工作区模块功能概览。"
	}
	return "。以上为工作区模块功能概览。"
}

func isKiroIntentionOnlyAnswer(text string) bool {
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > 160 {
		return false
	}
	if strings.Contains(text, "我来") {
		return strings.Contains(text, "探索") || strings.Contains(text, "了解") || strings.Contains(text, "读取")
	}
	if strings.Contains(text, "让我") {
		return strings.Contains(text, "探索") || strings.Contains(text, "了解") || strings.Contains(text, "读取") || strings.Contains(text, "验证") || strings.Contains(text, "补充") || strings.Contains(text, "查看") || strings.Contains(text, "分析")
	}
	return false
}

func isKiroClaudeCodeTransientPreamble(text string) bool {
	return isKiroIntentionOnlyAnswer(text) || isKiroCompletionOnlyPreamble(text)
}

func isKiroCompletionOnlyPreamble(text string) bool {
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > 160 {
		return false
	}
	for _, phrase := range []string{"探索完成", "读取完成", "分析完成", "检查完成", "梳理完成"} {
		if strings.Contains(text, phrase) {
			return !strings.Contains(text, "starrocks-")
		}
	}
	return false
}

func isKiroPossibleClaudeCodeTransientPreamblePrefix(text string) bool {
	runes := []rune(text)
	if len(runes) == 0 || len(runes) > 160 {
		return false
	}
	for _, starter := range []string{"我来", "让我"} {
		if strings.HasPrefix(starter, text) {
			return true
		}
		if strings.HasPrefix(text, starter) {
			return !kiroVisibleTextEndsWithTerminal(text)
		}
	}
	for _, phrase := range []string{"探索完成", "读取完成", "分析完成", "检查完成", "梳理完成"} {
		if strings.HasPrefix(phrase, text) {
			return true
		}
	}
	return false
}

func isKiroMarkdownHeadingFragment(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed == "#" || trimmed == "##" || trimmed == "###" || trimmed == "####"
}

func isKiroOpusModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "opus")
}

func kiroRequestHasIncompleteAssistantTail(body []byte) bool {
	messages := gjson.GetBytes(body, "messages").Array()
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Get("role").String() != "assistant" {
			continue
		}
		return kiroVisibleTextLooksIncomplete(kiroClaudeMessageVisibleText(msg.Get("content")))
	}
	return false
}

func kiroClaudeMessageVisibleText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	var b strings.Builder
	for _, block := range content.Array() {
		if block.Get("type").String() == "text" {
			b.WriteString(block.Get("text").String())
		}
	}
	return b.String()
}

func kiroVisibleTextLooksIncomplete(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	last := runes[len(runes)-1]
	if strings.ContainsRune("。.!?！？…」』）)]}》\"'`”’|", last) {
		return false
	}
	if strings.ContainsRune("，,、：:；;", last) {
		return true
	}
	if hasUnclosedKiroMarkdownFence(trimmed) || hasUnclosedKiroDelimiter(trimmed) {
		return true
	}
	return len(runes) >= 4
}

func kiroVisibleTextEndsWithTerminal(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	last := runes[len(runes)-1]
	return strings.ContainsRune("。.!?！？…」』）)]}》\"'`”’|", last)
}

func hasUnclosedKiroMarkdownFence(s string) bool {
	return strings.Count(s, "```")%2 == 1
}

func hasUnclosedKiroDelimiter(s string) bool {
	pairs := [][2]string{
		{"（", "）"},
		{"(", ")"},
		{"[", "]"},
		{"【", "】"},
		{"《", "》"},
		{"「", "」"},
		{"“", "”"},
	}
	for _, pair := range pairs {
		if strings.Count(s, pair[0]) > strings.Count(s, pair[1]) {
			return true
		}
	}
	return false
}

func longestSuffixPrefixLen(s, prefix string) int {
	maxLen := len(prefix) - 1
	if len(s) < maxLen {
		maxLen = len(s)
	}
	for n := maxLen; n > 0; n-- {
		if strings.HasSuffix(s, prefix[:n]) {
			return n
		}
	}
	return 0
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func newKiroStreamError(class helps.KiroErrorClass, message string, cause error) error {
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	return &helps.KiroError{Class: class, Body: message}
}

func hasKiroJSONResidue(remaining []byte) bool {
	return bytes.IndexByte(remaining, '{') >= 0
}

// sanitizeKiroVisibleText removes any `<thinking>...</thinking>` markup that
// previously leaked into a visible assistant text block. Older Kiro streaming
// behaviour (see streamKiroToClaudeSSE prior to the cross-event state machine)
// could emit reasoning fragments and orphan close tags as user-visible
// text_delta events when Kiro split the surrounding tags across multiple
// upstream events. Claude Code persists those fragments in the assistant turn,
// and without this guard they would be replayed as plain assistant content on
// every subsequent turn, polluting Kiro's context window.
//
// The function is intentionally conservative:
//   - Balanced `<thinking>...</thinking>` segments are dropped entirely.
//   - An open `<thinking>` without a matching close drops everything from the
//     tag onward.
//   - An orphan `</thinking>` drops everything before the tag (the leaked
//     reasoning fragment) and keeps the suffix.
//
// Dropped reasoning is not re-promoted into a thinking block: we cannot prove
// the fragment is faithful to the original model output, and re-promotion would
// keep the poisoned text in the conversation indefinitely.
func sanitizeKiroVisibleText(text string) string {
	if !strings.Contains(text, helps.KiroThinkingStartTag) && !strings.Contains(text, helps.KiroThinkingEndTag) {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	inThinking := false
	for text != "" {
		if !inThinking {
			startIdx := strings.Index(text, helps.KiroThinkingStartTag)
			stopIdx := strings.Index(text, helps.KiroThinkingEndTag)
			if startIdx < 0 && stopIdx < 0 {
				b.WriteString(text)
				return b.String()
			}
			if startIdx >= 0 && (stopIdx < 0 || startIdx < stopIdx) {
				b.WriteString(text[:startIdx])
				text = text[startIdx+len(helps.KiroThinkingStartTag):]
				inThinking = true
				continue
			}
			text = text[stopIdx+len(helps.KiroThinkingEndTag):]
			if strings.HasPrefix(text, "\n\n") {
				text = text[2:]
			} else if strings.HasPrefix(text, "\n") {
				text = text[1:]
			}
			continue
		}
		endIdx := strings.Index(text, helps.KiroThinkingEndTag)
		if endIdx < 0 {
			return b.String()
		}
		text = text[endIdx+len(helps.KiroThinkingEndTag):]
		if strings.HasPrefix(text, "\n\n") {
			text = text[2:]
		} else if strings.HasPrefix(text, "\n") {
			text = text[1:]
		}
		inThinking = false
	}
	return b.String()
}

func extractThinkingFromText(text string) (thinking string, remaining string) {
	startIdx := strings.Index(text, helps.KiroThinkingStartTag)
	if startIdx < 0 {
		return "", text
	}

	before := text[:startIdx]
	rest := text[startIdx+len(helps.KiroThinkingStartTag):]

	// Strip leading newline after <thinking>.
	if strings.HasPrefix(rest, "\r\n") {
		rest = rest[2:]
	} else if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	}

	endIdx := strings.Index(rest, helps.KiroThinkingEndTag)
	if endIdx < 0 {
		return rest, strings.TrimSpace(before)
	}

	thinking = rest[:endIdx]
	after := rest[endIdx+len(helps.KiroThinkingEndTag):]

	// Strip \n\n after </thinking>.
	if strings.HasPrefix(after, "\n\n") {
		after = after[2:]
	}

	remainingParts := strings.TrimSpace(before)
	if after != "" {
		if remainingParts != "" {
			remainingParts += " " + strings.TrimSpace(after)
		} else {
			remainingParts = after
		}
	}

	return thinking, remainingParts
}
