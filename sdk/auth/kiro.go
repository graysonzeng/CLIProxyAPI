package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// kiroRefreshLead is the duration before token expiry when refresh should occur.
// Kiro access tokens are short-lived (typically 1h); refresh 5 minutes early.
var kiroRefreshLead = 5 * time.Minute

// KiroAuthenticator implements the Authenticator interface for Kiro.
// Kiro does not support interactive login via CLI — users must obtain
// refresh tokens externally (e.g. from Kiro IDE) and place them in auths/kiro.json.
type KiroAuthenticator struct{}

// NewKiroAuthenticator constructs a new Kiro authenticator.
func NewKiroAuthenticator() Authenticator {
	return &KiroAuthenticator{}
}

// Provider returns the provider key for kiro.
func (KiroAuthenticator) Provider() string {
	return "kiro"
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (KiroAuthenticator) RefreshLead() *time.Duration {
	return &kiroRefreshLead
}

// Login is not supported for Kiro. Users must obtain tokens externally.
func (a KiroAuthenticator) Login(_ context.Context, _ *config.Config, _ *LoginOptions) (*coreauth.Auth, error) {
	return nil, fmt.Errorf("kiro: interactive login not supported; place kiro.json in auths/ directory")
}
