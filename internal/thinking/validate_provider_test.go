package thinking_test

import (
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
)

func TestValidateConfig_ProviderWrappedClaudeBudgetClampsForKiro(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID:   "claude-sonnet-4-5",
		Type: "kiro",
		Thinking: &registry.ThinkingSupport{
			Min:         1024,
			Max:         24576,
			ZeroAllowed: true,
		},
	}

	got, err := thinking.ValidateConfig(
		thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 31999},
		modelInfo,
		"claude",
		"claude",
		false,
	)
	if err != nil {
		t.Fatalf("ValidateConfig() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("ValidateConfig() returned nil config")
	}
	if got.Mode != thinking.ModeBudget || got.Budget != 24576 {
		t.Fatalf("validated config = %+v, want budget clamped to 24576", *got)
	}
}

func TestValidateConfig_NativeClaudeBudgetRemainsStrict(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID:   "claude-sonnet-4-5",
		Type: "claude",
		Thinking: &registry.ThinkingSupport{
			Min:         1024,
			Max:         24576,
			ZeroAllowed: true,
		},
	}

	_, err := thinking.ValidateConfig(
		thinking.ThinkingConfig{Mode: thinking.ModeBudget, Budget: 31999},
		modelInfo,
		"claude",
		"claude",
		false,
	)
	if err == nil {
		t.Fatal("ValidateConfig() error = nil, want budget out-of-range error")
	}
	var thinkingErr *thinking.ThinkingError
	if !errors.As(err, &thinkingErr) || thinkingErr.Code != thinking.ErrBudgetOutOfRange {
		t.Fatalf("ValidateConfig() error = %T %v, want ErrBudgetOutOfRange", err, err)
	}
}
