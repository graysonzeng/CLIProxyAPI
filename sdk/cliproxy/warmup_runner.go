package cliproxy

import (
	"context"
	"encoding/json"
	"math/rand"
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
	s.warmupRunner.update(newCfg.AuthProviderWarmup)
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

	refs := providerCfg.AuthIndexes
	if len(refs) == 0 {
		if _, err := r.executeWarmup(ctx, provider, model, prompt, nil); err != nil {
			log.WithError(err).Warnf("auth provider warmup failed (provider=%s model=%s)", provider, model)
			return false
		}
		log.Infof("auth provider warmup completed (provider=%s model=%s)", provider, model)
		return true
	}

	succeeded := false
	for _, ref := range refs {
		auth := r.authByRef(ref)
		if auth == nil {
			log.Warnf("auth provider warmup skipped missing auth ref (provider=%s model=%s)", provider, model)
			continue
		}
		if providerCfg.SkipWhenQuotaExceeded && authHasKnownQuotaBlock(auth) {
			log.Debugf("auth provider warmup skipped quota-blocked auth (provider=%s model=%s)", provider, model)
			continue
		}
		if _, err := r.executeWarmup(ctx, provider, model, prompt, auth); err != nil {
			log.WithError(err).Warnf("auth provider warmup failed (provider=%s model=%s)", provider, model)
			continue
		}
		log.Infof("auth provider warmup completed (provider=%s model=%s)", provider, model)
		succeeded = true
	}
	return succeeded
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
