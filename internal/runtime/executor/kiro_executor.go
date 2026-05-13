package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// CountTokens is not natively supported by Kiro; returns unsupported error.
func (e *KiroExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "kiro: count tokens not supported"}
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
	cwReq, toolNameMaps, err := buildKiroCodeWhispererRequest(body, auth)
	if err != nil {
		return resp, fmt.Errorf("kiro executor: %w", err)
	}

	region := kiroMetaStr(auth, "region")
	url := helps.KiroBaseURL(region)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(cwReq))
	if err != nil {
		return resp, fmt.Errorf("kiro executor: %w", err)
	}
	applyKiroHTTPHeaders(httpReq, auth)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: response body close error: %v", errClose)
		}
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		return resp, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

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

	cwReq, toolNameMaps, err := buildKiroCodeWhispererRequest(body, auth)
	if err != nil {
		return nil, fmt.Errorf("kiro executor: %w", err)
	}

	region := kiroMetaStr(auth, "region")
	url := helps.KiroBaseURL(region)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(cwReq))
	if err != nil {
		return nil, fmt.Errorf("kiro executor: %w", err)
	}
	applyKiroHTTPHeaders(httpReq, auth)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: response body close error: %v", errClose)
		}
		return nil, statusErr{code: httpResp.StatusCode, msg: string(b)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kiro executor: response body close error: %v", errClose)
			}
		}()

		if from == to {
			// Claude→Claude: forward SSE lines directly.
			streamKiroToClaudeSSE(ctx, httpResp.Body, toolNameMaps, baseModel, func(line []byte) {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: append(line, '\n')}:
				case <-ctx.Done():
				}
			})
		} else {
			// Other formats: translate each SSE line.
			var param any
			streamKiroToClaudeSSE(ctx, httpResp.Body, toolNameMaps, baseModel, func(line []byte) {
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
		reporter.EnsurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// --- Internal helpers ---

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
func buildKiroCodeWhispererRequest(claudePayload []byte, auth *cliproxyauth.Auth) ([]byte, *helps.KiroToolNameMaps, error) {
	conversationID := uuid.New().String()
	system := gjson.GetBytes(claudePayload, "system")
	messagesRaw := gjson.GetBytes(claudePayload, "messages")
	toolsRaw := gjson.GetBytes(claudePayload, "tools")
	thinkingRaw := gjson.GetBytes(claudePayload, "thinking")
	modelRaw := gjson.GetBytes(claudePayload, "model").String()

	cwModel := helps.MapKiroModel(modelRaw)

	// Extract system prompt text.
	systemPrompt := extractSystemPromptText(system)

	// Generate thinking prefix.
	if thinkingRaw.Exists() {
		tType := thinkingRaw.Get("type").String()
		budgetTokens := int(thinkingRaw.Get("budget_tokens").Int())
		effort := thinkingRaw.Get("effort").String()
		if effort == "" {
			// Claude canonical format places effort under output_config.effort.
			effort = gjson.GetBytes(claudePayload, "output_config.effort").String()
		}
		prefix := helps.GenerateKiroThinkingPrefix(tType, budgetTokens, effort)
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
				textParts = append(textParts, part.Get("text").String())
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
		arm["content"] = content.String()
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
	events, _ := helps.ParseAwsEventStreamBuffer(string(rawResp))

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
	streamKiroToClaudeSSE(context.Background(), bytes.NewReader(rawResp), toolNameMaps, model, func(line []byte) {
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

// streamKiroToClaudeSSE incrementally reads a Kiro streaming response, parsing
// AWS Event Stream chunks as they arrive and converting each event to Claude SSE
// format. The emit callback is called for each SSE line as soon as it is ready,
// enabling true incremental streaming to the client.
func streamKiroToClaudeSSE(_ context.Context, body io.Reader, toolNameMaps *helps.KiroToolNameMaps, model string, emit func([]byte)) {
	msgID := "msg_" + uuid.New().String()
	nextBlockIndex := 0
	stoppedBlocks := make(map[int]bool)
	thinkingBlockIndex := -1
	textBlockIndex := -1
	var lastContent string

	type activeToolUse struct {
		name      string
		toolUseID string
		input     string
		index     int
	}
	var activeTool *activeToolUse

	// Helper to emit an SSE line.
	emitSSE := func(eventType string, data interface{}) {
		jsonData, _ := json.Marshal(data)
		line := fmt.Sprintf("event: %s\ndata: %s\n", eventType, string(jsonData))
		emit([]byte(line))
	}

	// message_start
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

	// processEvent handles a single parsed Kiro stream event.
	processEvent := func(evt helps.KiroStreamEvent) {
		switch evt.Type {
		case "content":
			if evt.Content == lastContent {
				return
			}
			lastContent = evt.Content

			// Check for thinking tags in content.
			if strings.Contains(evt.Content, helps.KiroThinkingStartTag) {
				thinkingText, afterText := extractThinkingFromText(evt.Content)
				if thinkingText != "" && thinkingBlockIndex < 0 {
					idx := nextBlockIndex
					nextBlockIndex++
					thinkingBlockIndex = idx
					emitSSE("content_block_start", map[string]interface{}{
						"type":          "content_block_start",
						"index":         idx,
						"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
					})
					emitSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": idx,
						"delta": map[string]interface{}{"type": "thinking_delta", "thinking": thinkingText},
					})
					stopBlock(idx)
				}
				if afterText != "" {
					if textBlockIndex < 0 {
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
						"delta": map[string]interface{}{"type": "text_delta", "text": afterText},
					})
				}
			} else {
				// Plain text content.
				if textBlockIndex < 0 {
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
					"delta": map[string]interface{}{"type": "text_delta", "text": evt.Content},
				})
			}

		case "toolUse":
			hasToolCalls = true

			// If the same toolUseID is already active, treat this as an input continuation
			// rather than a new tool call. Kiro may send multiple events with `name` set
			// for the same tool call (input split across chunks).
			if activeTool != nil && activeTool.toolUseID == evt.ToolUseID {
				if evt.ToolInput != "" {
					activeTool.input += evt.ToolInput
					emitSSE("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeTool.index,
						"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": evt.ToolInput},
					})
				}
				if evt.ToolStop {
					stopBlock(activeTool.index)
					activeTool = nil
				}
				return
			}

			// Stop text block if open.
			if textBlockIndex >= 0 {
				stopBlock(textBlockIndex)
			}
			if activeTool != nil {
				// Stop previous tool (different toolUseID).
				stopBlock(activeTool.index)
			}

			name := evt.ToolName
			if toolNameMaps != nil {
				name = toolNameMaps.FromKiroName(name)
			}
			idx := nextBlockIndex
			nextBlockIndex++
			activeTool = &activeToolUse{name: name, toolUseID: evt.ToolUseID, input: evt.ToolInput, index: idx}

			emitSSE("content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": idx,
				"content_block": map[string]interface{}{
					"type": "tool_use",
					"id":   evt.ToolUseID,
					"name": name,
				},
			})
			if evt.ToolInput != "" {
				emitSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": evt.ToolInput},
				})
			}
			if evt.ToolStop {
				stopBlock(idx)
				activeTool = nil
			}

		case "toolUseInput":
			if activeTool != nil {
				activeTool.input += evt.ToolInput
				emitSSE("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeTool.index,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": evt.ToolInput},
				})
			}

		case "toolUseStop":
			if activeTool != nil {
				stopBlock(activeTool.index)
				activeTool = nil
			}
		}
	}

	// Incrementally read from the response body and parse events.
	const readBufSize = 32 * 1024
	readBuf := make([]byte, readBufSize)
	var remaining string
	for {
		n, readErr := body.Read(readBuf)
		if n > 0 {
			remaining += string(readBuf[:n])
			var events []helps.KiroStreamEvent
			events, remaining = helps.ParseAwsEventStreamBuffer(remaining)
			for _, evt := range events {
				processEvent(evt)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Warnf("kiro executor: error reading stream: %v", readErr)
			}
			break
		}
	}
	// Final parse attempt on any remaining buffer data.
	if remaining != "" {
		finalEvents, _ := helps.ParseAwsEventStreamBuffer(remaining)
		for _, evt := range finalEvents {
			processEvent(evt)
		}
	}

	// Stop any remaining open blocks.
	if thinkingBlockIndex >= 0 {
		stopBlock(thinkingBlockIndex)
	}
	if textBlockIndex >= 0 {
		stopBlock(textBlockIndex)
	}
	if activeTool != nil {
		stopBlock(activeTool.index)
	}

	stopReason := "end_turn"
	if hasToolCalls {
		stopReason = "tool_use"
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
