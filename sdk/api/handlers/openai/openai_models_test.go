package openai

import "testing"

func TestSupportedEndpointTypes(t *testing.T) {
	tests := []struct {
		name    string
		model   map[string]any
		expects string
	}{
		{name: "anthropic", model: map[string]any{"owned_by": "anthropic"}, expects: "anthropic"},
		{name: "anthropic case insensitive", model: map[string]any{"owned_by": "Anthropic"}, expects: "anthropic"},
		{name: "openai", model: map[string]any{"owned_by": "openai"}, expects: "openai"},
		{name: "other providers use openai-compatible route", model: map[string]any{"owned_by": "xai"}, expects: "openai"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := supportedEndpointTypes(tt.model)
			if len(got) != 1 || got[0] != tt.expects {
				t.Fatalf("supportedEndpointTypes(%v) = %v, want [%s]", tt.model, got, tt.expects)
			}
		})
	}
}
