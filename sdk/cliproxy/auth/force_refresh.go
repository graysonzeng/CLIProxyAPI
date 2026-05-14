package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// AuthRefreshFunc forces an immediate provider Refresh of the auth identified by
// authID and persists the result through the manager. It returns the refreshed
// auth snapshot when refresh succeeds. Callers (typically provider executors
// performing a bounded in-flight retry) should swap their local auth handle's
// secret material with the returned snapshot before retrying upstream.
//
// Semantics differ from Manager.RefreshAuthByID in that ForceRefresh bypasses
// the scheduled-refresh evaluator: it is intended for credentials that just
// failed authorization (e.g. upstream 401) and need to be rotated regardless
// of the freshness window the auto-refresh loop normally enforces.
type AuthRefreshFunc func(ctx context.Context, authID string) (*Auth, error)

// forceRefreshAuthCtxKey is the unexported context key used to convey the
// force-refresh callback from the manager to provider executors. Using an
// unexported struct guarantees no accidental collision with other context
// values.
type forceRefreshAuthCtxKey struct{}

// WithForceRefreshAuth returns a derived context that carries fn as the
// in-flight force-refresh callback. Provider executors can retrieve fn via
// ForceRefreshAuthFromContext when they need to refresh and persist auth in
// the middle of a request (for example after receiving an upstream 401).
func WithForceRefreshAuth(ctx context.Context, fn AuthRefreshFunc) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, forceRefreshAuthCtxKey{}, fn)
}

// ForceRefreshAuthFromContext returns the AuthRefreshFunc previously installed
// by WithForceRefreshAuth, or nil when the context carries no callback.
func ForceRefreshAuthFromContext(ctx context.Context) AuthRefreshFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(forceRefreshAuthCtxKey{}).(AuthRefreshFunc)
	return fn
}

// ForceRefreshAuth forces an immediate refresh of the auth identified by id,
// persists the refreshed snapshot through Manager.Update (which writes to the
// configured Store and updates the in-memory map), and returns the latest
// snapshot. Unlike RefreshAuthByID this does not consult the freshness
// evaluator, so it is suitable for the in-flight 401 retry path where the
// caller has direct evidence that the current credential is no longer valid.
//
// Concurrency note: this method intentionally does not coalesce concurrent
// callers via markRefreshPending. Coalescing concurrent 401-driven refreshes
// is tracked as P1 (proactive refresh + locking) in the Kiro reliability
// review; callers must tolerate redundant refreshes for now.
func (m *Manager) ForceRefreshAuth(ctx context.Context, id string) (*Auth, error) {
	if m == nil {
		return nil, &Error{Code: "manager_not_available", Message: "auth manager unavailable", HTTPStatus: http.StatusInternalServerError}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, &Error{Code: "auth_not_found", Message: "auth id is required", HTTPStatus: http.StatusNotFound}
	}

	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	var cloned *Auth
	if auth != nil {
		exec = m.executors[auth.Provider]
		cloned = auth.Clone()
	}
	m.mu.RUnlock()
	if cloned == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth not found", HTTPStatus: http.StatusNotFound}
	}
	if exec == nil {
		return cloned, &Error{Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusConflict}
	}

	updated, refreshErr := exec.Refresh(ctx, cloned)
	if refreshErr != nil {
		// Reuse the same failure bookkeeping as the scheduled refresh path so
		// the auto-refresh loop and selector see consistent state.
		log.WithFields(log.Fields{
			"event":    "force_refresh_failed",
			"provider": auth.Provider,
			"auth_id":  id,
		}).Debugf("ForceRefreshAuth: refresh returned error: %v", refreshErr)
		now := time.Now()
		unauthorized := isUnauthorizedError(refreshErr)
		var snapshot *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.LastError = refreshErrorFromError(refreshErr)
			if unauthorized {
				current.NextRefreshAfter = time.Time{}
				current.Unavailable = true
				current.Status = StatusError
				current.StatusMessage = "unauthorized"
			} else {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			}
			m.auths[id] = current
			snapshot = current.Clone()
			if m.scheduler != nil {
				m.scheduler.upsertAuth(current.Clone())
			}
		}
		m.mu.Unlock()
		return snapshot, refreshErr
	}
	if updated == nil {
		updated = cloned
	}
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	now := time.Now()
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.Unavailable = false
	updated.StatusMessage = ""
	if updated.Status == StatusError {
		updated.Status = StatusActive
	}
	updated.UpdatedAt = now
	if m.shouldRefresh(updated, now) {
		updated.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}

	stored, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return stored, errUpdate
	}
	return stored, nil
}
