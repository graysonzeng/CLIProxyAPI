package config

import "testing"

func TestParseConfigBytesAuthProviderWarmupNormalizesDefaults(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`auth-provider-warmup:
  enabled: true
  providers:
    - provider: Codex
      enabled: true
      model: gpt-5.3-codex
      auth-indexes: ["0001", "codex-auth", "0001"]
      skip-when-quota-exceeded: true
    - provider: unsupported
      enabled: true
      model: nope
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes returned error: %v", err)
	}
	warmup := cfg.AuthProviderWarmup
	if !warmup.Enabled {
		t.Fatalf("expected warmup enabled")
	}
	if warmup.Interval != AuthProviderWarmupDefaultInterval {
		t.Fatalf("interval = %q, want %q", warmup.Interval, AuthProviderWarmupDefaultInterval)
	}
	if warmup.Jitter != AuthProviderWarmupDefaultJitter {
		t.Fatalf("jitter = %q, want %q", warmup.Jitter, AuthProviderWarmupDefaultJitter)
	}
	if warmup.Prompt != AuthProviderWarmupDefaultPrompt {
		t.Fatalf("prompt = %q, want %q", warmup.Prompt, AuthProviderWarmupDefaultPrompt)
	}
	if warmup.MaxConcurrency != AuthProviderWarmupDefaultMaxConcurrency {
		t.Fatalf("max concurrency = %d, want %d", warmup.MaxConcurrency, AuthProviderWarmupDefaultMaxConcurrency)
	}
	if len(warmup.Providers) != 1 {
		t.Fatalf("provider count = %d, want 1", len(warmup.Providers))
	}
	provider := warmup.Providers[0]
	if provider.Provider != "codex" {
		t.Fatalf("provider = %q, want codex", provider.Provider)
	}
	if !provider.SkipWhenQuotaExceeded {
		t.Fatalf("expected skip-when-quota-exceeded to parse as true")
	}
	wantIndexes := []string{"0001", "codex-auth"}
	if len(provider.AuthIndexes) != len(wantIndexes) {
		t.Fatalf("auth-indexes = %#v, want %#v", provider.AuthIndexes, wantIndexes)
	}
	for i := range wantIndexes {
		if provider.AuthIndexes[i] != wantIndexes[i] {
			t.Fatalf("auth-indexes = %#v, want %#v", provider.AuthIndexes, wantIndexes)
		}
	}
}

func TestIsWarmupSupportedProvider(t *testing.T) {
	for _, provider := range []string{"kiro", "codex", "claude", "antigravity", " CODEX "} {
		if !IsWarmupSupportedProvider(provider) {
			t.Fatalf("expected %q to be supported", provider)
		}
	}
	if IsWarmupSupportedProvider("gemini") {
		t.Fatalf("gemini should not be supported yet")
	}
}
