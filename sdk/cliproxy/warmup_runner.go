package cliproxy

import (
	"context"
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

var authProviderWarmupRetryInterval = 5 * time.Second

// providersWithDefaultWarmup lists providers that get an auto-injected
// warmup entry whenever the operator enabled the warmup feature but did not
// explicitly configure that provider, AND the auth manager has at least one
// healthy auth for that provider registered.
//
// The auto-injection serves two goals:
//  1. Warm the provider executor's shared HTTP client connection pool at
//     server start (kills the ~800ms cold-handshake tax on the first user
//     request after a restart — measured for Kiro on a us-east-1 endpoint).
//  2. Hold the connection alive across long idle gaps via the periodic
//     warmup interval (default 1h, with jitter), so users coming back to a
//     quiet proxy do not pay the cold tax either.
//
// Operators can override by listing the provider explicitly in
// auth-provider-warmup.providers (their entry takes precedence and the
// default is suppressed). Disabling the whole feature
// (auth-provider-warmup.enabled=false) suppresses auto-injection too — the
// startup ping is conceptually part of the same warmup contract, so a single
// switch controls both.
//
// The map value is the model to ping for that provider. Use Sonnet for Kiro
// because Haiku's tool-use stream has produced incomplete input shards during
// Claude Code auto-routing and should not be used by default background paths.
// Adding a new provider here is opt-in: other providers (anthropic, gemini,
// openai-compat, etc.) currently rely on the operator's explicit config.
var providersWithDefaultWarmup = map[string]string{
	"kiro": "claude-sonnet-4-6",
}

// appendDefaultWarmupProviders augments the operator's warmup config with
// per-provider defaults for any provider listed in
// providersWithDefaultWarmup that has at least one healthy auth registered
// but no explicit entry in the operator config.
//
// Returns the cfg unchanged when:
//   - cfg.Enabled is false (operator disabled the whole feature),
//   - manager is nil (no auth manager available, e.g. early bootstrap),
//   - the operator already configured every default provider explicitly,
//   - none of the default providers have a healthy auth registered.
//
// The returned config is a shallow copy when augmented; the caller may pass
// it to update() without affecting the operator's source config struct.
func appendDefaultWarmupProviders(cfg internalconfig.AuthProviderWarmupConfig, manager *coreauth.Manager) internalconfig.AuthProviderWarmupConfig {
	if !cfg.Enabled || manager == nil {
		return cfg
	}

	have := make(map[string]struct{}, len(cfg.Providers))
	for _, p := range cfg.Providers {
		key := strings.ToLower(strings.TrimSpace(p.Provider))
		if key == "" {
			continue
		}
		have[key] = struct{}{}
	}

	activeProviders := make(map[string]struct{})
	for _, auth := range manager.List() {
		if auth == nil || auth.Status != coreauth.StatusActive {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(auth.Provider))
		if key == "" {
			continue
		}
		activeProviders[key] = struct{}{}
	}

	// Stable iteration order so the appended entries are deterministic
	// across server restarts (helps log readability and test assertions).
	added := false
	augmented := cfg
	for _, provider := range sortedKeys(providersWithDefaultWarmup) {
		if _, ok := have[provider]; ok {
			continue
		}
		if _, ok := activeProviders[provider]; !ok {
			continue
		}
		if !added {
			augmented.Providers = append([]internalconfig.WarmupProviderConfig(nil), cfg.Providers...)
			added = true
		}
		augmented.Providers = append(augmented.Providers, internalconfig.WarmupProviderConfig{
			Provider:              provider,
			Model:                 providersWithDefaultWarmup[provider],
			Enabled:               true,
			SkipWhenQuotaExceeded: true,
		})
		log.WithFields(log.Fields{
			"provider": provider,
			"model":    providersWithDefaultWarmup[provider],
		}).Debug("auth provider warmup: auto-injected default provider entry")
	}
	return augmented
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type authProviderWarmupRunner struct {
	service *Service

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func newAuthProviderWarmupRunner(service *Service) *authProviderWarmupRunner {
	return &authProviderWarmupRunner{service: service}
}

func (s *Service) applyAuthProviderWarmupConfig(newCfg *config.Config) {
	if s == nil {
		return
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupRunner == nil {
		s.warmupRunner = newAuthProviderWarmupRunner(s)
	}
	if newCfg == nil || newCfg.Home.Enabled {
		s.warmupRunner.update(internalconfig.AuthProviderWarmupConfig{})
		return
	}
	// Augment with per-provider defaults (e.g. kiro startup connection
	// pool warm-up) before handing to the runner. The augmenter respects
	// the operator's explicit provider list and only fills gaps.
	augmented := appendDefaultWarmupProviders(newCfg.AuthProviderWarmup, s.coreManager)
	s.warmupRunner.update(augmented)
}

func (s *Service) stopAuthProviderWarmup() {
	_ = s.stopAuthProviderWarmupWithContext(context.Background())
}

func (s *Service) stopAuthProviderWarmupWithContext(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupRunner == nil {
		return nil
	}
	return s.warmupRunner.stopWithContext(ctx)
}

func (r *authProviderWarmupRunner) update(cfg internalconfig.AuthProviderWarmupConfig) {
	cfg.Normalize()
	providers := enabledWarmupProviders(cfg)

	r.mu.Lock()
	if err := r.stopLocked(context.Background()); err != nil {
		log.WithError(err).Warn("failed to stop existing auth provider warmup scheduler")
	}
	if !cfg.Enabled || len(providers) == 0 {
		r.mu.Unlock()
		log.Debug("auth provider warmup scheduler stopped")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done
	r.mu.Unlock()

	log.Infof("auth provider warmup scheduler started (providers=%d, interval=%s)", len(providers), cfg.Interval)
	go func() {
		defer close(done)
		r.run(ctx, cfg)
	}()
}

func (r *authProviderWarmupRunner) stop() {
	_ = r.stopWithContext(context.Background())
}

func (r *authProviderWarmupRunner) stopWithContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopLocked(ctx)
}

func (r *authProviderWarmupRunner) stopLocked(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cancel := r.cancel
	done := r.done
	r.cancel = nil
	r.done = nil
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (r *authProviderWarmupRunner) run(ctx context.Context, cfg internalconfig.AuthProviderWarmupConfig) {
	cfg.Normalize()
	providers := enabledWarmupProviders(cfg)
	if len(providers) == 0 {
		return
	}
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = internalconfig.AuthProviderWarmupDefaultMaxConcurrency
	}
	sem := make(chan struct{}, maxConcurrency)

	var wg sync.WaitGroup
	for _, providerCfg := range providers {
		providerCfg := providerCfg
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runProviderLoop(ctx, cfg, providerCfg, sem)
		}()
	}
	wg.Wait()
}

func (r *authProviderWarmupRunner) runProviderLoop(ctx context.Context, cfg internalconfig.AuthProviderWarmupConfig, providerCfg internalconfig.WarmupProviderConfig, sem chan struct{}) {
	ok := r.runProvider(ctx, cfg, providerCfg, sem)
	for {
		delay := warmupProviderInterval(cfg, providerCfg) + warmupProviderJitter(cfg, providerCfg)
		if !ok {
			delay = authProviderWarmupRetryInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			ok = r.runProvider(ctx, cfg, providerCfg, sem)
		}
	}
}

func (r *authProviderWarmupRunner) runOnce(ctx context.Context, cfg internalconfig.AuthProviderWarmupConfig) {
	cfg.Normalize()
	maxConcurrency := cfg.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = internalconfig.AuthProviderWarmupDefaultMaxConcurrency
	}
	sem := make(chan struct{}, maxConcurrency)
	for _, providerCfg := range enabledWarmupProviders(cfg) {
		r.runProvider(ctx, cfg, providerCfg, sem)
	}
}

func (r *authProviderWarmupRunner) runProvider(ctx context.Context, cfg internalconfig.AuthProviderWarmupConfig, providerCfg internalconfig.WarmupProviderConfig, sem chan struct{}) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return false
	}

	provider := strings.ToLower(strings.TrimSpace(providerCfg.Provider))
	model := strings.TrimSpace(providerCfg.Model)
	if provider == "" || model == "" || r == nil || r.service == nil || r.service.coreManager == nil {
		return false
	}

	prompt := strings.TrimSpace(providerCfg.Prompt)
	if prompt == "" {
		prompt = strings.TrimSpace(cfg.Prompt)
	}
	if prompt == "" {
		prompt = internalconfig.AuthProviderWarmupDefaultPrompt
	}

	auths := r.resolveWarmupAuths(provider, providerCfg)
	if len(auths) == 0 {
		log.Debugf("auth provider warmup: no eligible auth registered (provider=%s model=%s)", provider, model)
		return false
	}

	succeeded := false
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		if providerCfg.SkipWhenQuotaExceeded && authHasKnownQuotaBlock(auth) {
			log.Debugf("auth provider warmup skipped quota-blocked auth (provider=%s model=%s auth=%s)", provider, model, auth.ID)
			continue
		}
		if _, err := r.executeWarmup(ctx, provider, model, prompt, auth); err != nil {
			log.WithError(err).Warnf("auth provider warmup failed (provider=%s model=%s auth=%s)", provider, model, auth.ID)
			continue
		}
		log.Infof("auth provider warmup completed (provider=%s model=%s auth=%s)", provider, model, auth.ID)
		succeeded = true
	}
	return succeeded
}

// resolveWarmupAuths returns the auths to warm for one provider/cycle.
//
// When the operator listed explicit auth refs in providerCfg.AuthIndexes the
// resolver honors that exact list (intentional pinning, e.g. paid vs free
// account selection).
//
// When the operator left auth-indexes empty the resolver enumerates EVERY
// healthy auth registered for that provider so multi-account setups
// (codex queue rotation, multiple kiro accounts, an Anthropic key pool,
// etc.) keep both the active and the candidate accounts warm. Without this
// pass the proxy would only warm the currently active candidate, so
// fail-over to a queued account paid the ~800ms cold-handshake tax on the
// first request and could surface as visible latency in Claude Code.
//
// The returned auths are clones of the manager state. Quota gating is
// applied by the caller via authHasKnownQuotaBlock so the operator's
// SkipWhenQuotaExceeded flag still wins.
func (r *authProviderWarmupRunner) resolveWarmupAuths(provider string, providerCfg internalconfig.WarmupProviderConfig) []*coreauth.Auth {
	if r == nil || r.service == nil || r.service.coreManager == nil {
		return nil
	}
	if len(providerCfg.AuthIndexes) > 0 {
		out := make([]*coreauth.Auth, 0, len(providerCfg.AuthIndexes))
		for _, ref := range providerCfg.AuthIndexes {
			auth := r.authByRef(ref)
			if auth == nil {
				log.Warnf("auth provider warmup: missing auth ref (provider=%s ref=%s)", provider, ref)
				continue
			}
			out = append(out, auth)
		}
		return out
	}
	return r.healthyAuthsForProvider(provider)
}

// healthyAuthsForProvider returns clones of every healthy auth for the
// given provider. Healthy = registered + not Disabled + Status==Active.
// Quota gating is intentionally NOT done here so the caller can apply the
// per-provider SkipWhenQuotaExceeded flag.
func (r *authProviderWarmupRunner) healthyAuthsForProvider(provider string) []*coreauth.Auth {
	target := strings.ToLower(strings.TrimSpace(provider))
	if target == "" || r == nil || r.service == nil || r.service.coreManager == nil {
		return nil
	}
	var out []*coreauth.Auth
	for _, auth := range r.service.coreManager.List() {
		if auth == nil || auth.Disabled {
			continue
		}
		if auth.Status != coreauth.StatusActive {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), target) {
			continue
		}
		auth.EnsureIndex()
		out = append(out, auth)
	}
	return out
}

func (r *authProviderWarmupRunner) executeWarmup(ctx context.Context, provider, model, prompt string, pinned *coreauth.Auth) (string, error) {
	type result struct {
		selectedAuthID string
		err            error
	}
	ch := make(chan result, 1)
	go func() {
		selectedAuthID, errExec := r.executeWarmupBlocking(ctx, provider, model, prompt, pinned)
		ch <- result{selectedAuthID: selectedAuthID, err: errExec}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-ch:
		return res.selectedAuthID, res.err
	}
}

func (r *authProviderWarmupRunner) executeWarmupBlocking(ctx context.Context, provider, model, prompt string, pinned *coreauth.Auth) (string, error) {
	payload, errPayload := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages": []map[string]string{{
			"role":    "user",
			"content": prompt,
		}},
	})
	if errPayload != nil {
		return "", errPayload
	}

	selectedAuthID := ""
	metadata := map[string]any{
		cliproxyexecutor.SyntheticRequestMetadataKey: cliproxyexecutor.SyntheticRequestKindWarmup,
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthID = strings.TrimSpace(authID)
		},
	}
	if pinned != nil {
		metadata[cliproxyexecutor.PinnedAuthMetadataKey] = pinned.ID
	}

	_, errExec := r.service.coreManager.Execute(ctx, []string{provider}, cliproxyexecutor.Request{
		Model:    model,
		Payload:  payload,
		Format:   sdktranslator.FormatClaude,
		Metadata: metadata,
	}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Metadata:        metadata,
	})
	if selectedAuthID == "" && pinned != nil {
		selectedAuthID = pinned.ID
	}
	return selectedAuthID, errExec
}

func (r *authProviderWarmupRunner) authByRef(ref string) *coreauth.Auth {
	ref = strings.TrimSpace(ref)
	if ref == "" || r == nil || r.service == nil || r.service.coreManager == nil {
		return nil
	}
	if auth, ok := r.service.coreManager.GetByID(ref); ok && auth != nil {
		return auth
	}
	for _, auth := range r.service.coreManager.List() {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == ref {
			return auth
		}
	}
	return nil
}

func enabledWarmupProviders(cfg internalconfig.AuthProviderWarmupConfig) []internalconfig.WarmupProviderConfig {
	if !cfg.Enabled {
		return nil
	}
	out := make([]internalconfig.WarmupProviderConfig, 0, len(cfg.Providers))
	for _, providerCfg := range cfg.Providers {
		if !providerCfg.Enabled {
			continue
		}
		if strings.TrimSpace(providerCfg.Provider) == "" || strings.TrimSpace(providerCfg.Model) == "" {
			continue
		}
		out = append(out, providerCfg)
	}
	return out
}

func warmupProviderInterval(cfg internalconfig.AuthProviderWarmupConfig, providerCfg internalconfig.WarmupProviderConfig) time.Duration {
	value := strings.TrimSpace(providerCfg.Interval)
	if value == "" {
		value = strings.TrimSpace(cfg.Interval)
	}
	if value == "" {
		value = internalconfig.AuthProviderWarmupDefaultInterval
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		fallback, _ := time.ParseDuration(internalconfig.AuthProviderWarmupDefaultInterval)
		return fallback
	}
	return parsed
}

func warmupProviderJitter(cfg internalconfig.AuthProviderWarmupConfig, providerCfg internalconfig.WarmupProviderConfig) time.Duration {
	value := strings.TrimSpace(providerCfg.Jitter)
	if value == "" {
		value = strings.TrimSpace(cfg.Jitter)
	}
	if value == "" {
		return 0
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(parsed)))
}

func authHasKnownQuotaBlock(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Unavailable || auth.Quota.Exceeded {
		return true
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.Unavailable || state.Quota.Exceeded {
			return true
		}
	}
	return false
}
