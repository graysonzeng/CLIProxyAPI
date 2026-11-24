package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func testClaudeOAuthAuth(id string, priority string) *Auth {
	attrs := map[string]string{}
	if priority != "" {
		attrs["priority"] = priority
	}
	return &Auth{
		ID:         id,
		Provider:   "claude",
		Attributes: attrs,
		Metadata:   map[string]any{"email": "graysonzeng123@example.com"},
	}
}

func testOpenAICompatAuth(id, compatName, priority string) *Auth {
	attrs := map[string]string{"compat_name": compatName}
	if priority != "" {
		attrs["priority"] = priority
	}
	return &Auth{
		ID:         id,
		Provider:   "openai-compatibility",
		Attributes: attrs,
	}
}

func idsOf(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			ids = append(ids, "<nil>")
			continue
		}
		ids = append(ids, auth.ID)
	}
	return ids
}

func TestGetSelectorAvailableAuthsPrefersOAuthOnNewPickPool(t *testing.T) {
	t.Parallel()
	oauth := testClaudeOAuthAuth("claude-oauth", "0")
	compat := testOpenAICompatAuth("openai-compatibility:opencode2:eb0afaffa008", "opencode2", "0")
	got, err := getSelectorAvailableAuths(context.Background(), []*Auth{compat, oauth}, "claude", "", time.Now())
	if err != nil {
		t.Fatalf("getSelectorAvailableAuths: %v", err)
	}
	if len(got) != 1 || got[0] == nil || got[0].ID != "claude-oauth" {
		t.Fatalf("getSelectorAvailableAuths = %#v, want only claude oauth for first-pick pool", idsOf(got))
	}
}

func TestWeightedRoundRobinNewSessionPrefersClaudeOAuthOverCompat(t *testing.T) {
	t.Parallel()
	oauth := testClaudeOAuthAuth("claude-oauth", "0")
	compat := testOpenAICompatAuth("openai-compatibility:opencode2:eb0afaffa008", "opencode2", "0")
	available, err := getSelectorAvailableAuths(context.Background(), []*Auth{compat, oauth}, "claude", "claude-haiku-5-5", time.Now())
	if err != nil {
		t.Fatalf("getSelectorAvailableAuths: %v", err)
	}
	selector := &WeightedRoundRobinSelector{}
	for i := 0; i < 16; i++ {
		selected, errPick := selector.Pick(context.Background(), "claude", "claude-haiku-5-5", cliproxyexecutor.Options{}, available)
		if errPick != nil {
			t.Fatalf("Pick #%d: %v", i, errPick)
		}
		if selected == nil || selected.ID != "claude-oauth" {
			t.Fatalf("Pick #%d selected %#v, want claude-oauth first bind", i, selected)
		}
	}
}

func TestGetSelectorAvailableAuthsKeepsCompatWhenNoOAuth(t *testing.T) {
	t.Parallel()
	compat := testOpenAICompatAuth("openai-compatibility:opencode2:eb0afaffa008", "opencode2", "0")
	got, err := getSelectorAvailableAuths(context.Background(), []*Auth{compat}, "claude", "", time.Now())
	if err != nil {
		t.Fatalf("getSelectorAvailableAuths: %v", err)
	}
	if len(got) != 1 || got[0].ID != compat.ID {
		t.Fatalf("getSelectorAvailableAuths = %#v, want compat fallback when no oauth remains", idsOf(got))
	}
}

func TestHighestPriorityAuthsHonorsExplicitHigherCompatPriority(t *testing.T) {
	t.Parallel()
	oauth := testClaudeOAuthAuth("claude-oauth", "0")
	compat := testOpenAICompatAuth("openai-compatibility:opencode2:eb0afaffa008", "opencode2", "10")
	got, err := getSelectorAvailableAuths(context.Background(), []*Auth{compat, oauth}, "claude", "", time.Now())
	if err != nil {
		t.Fatalf("getSelectorAvailableAuths: %v", err)
	}
	if len(got) != 1 || got[0].ID != compat.ID {
		t.Fatalf("getSelectorAvailableAuths = %#v, want higher explicit compat priority", idsOf(got))
	}
}
