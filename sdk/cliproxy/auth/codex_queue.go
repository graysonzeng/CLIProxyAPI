package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codexjwt "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Codex queue state names. Kept as plain strings so the management API can
// surface them verbatim without coupling to runtime constants.
const (
	CodexQueueStateActive          = "active"
	CodexQueueStateStandby         = "standby"
	CodexQueueStateSwitchPending   = "switch_pending"
	CodexQueueStateManagedDisabled = "managed_disabled"
	CodexQueueStateManualDisabled  = "manual_disabled"
	CodexQueueStateQuotaUnknown    = "quota_unknown"
	CodexQueueStateQuotaError      = "quota_error"
	CodexQueueStateIneligibleGroup = "ineligible_group"

	CodexQuotaStatusKnown   = "known"
	CodexQuotaStatusUnknown = "unknown"
	CodexQuotaStatusError   = "error"

	CodexQueueDisableReasonLowQuota = "low_quota_after_idle"
	CodexQueueDisableReasonManual   = "manual"

	CodexQueueSwitchReasonLowQuota     = "low_quota_below_threshold"
	CodexQueueSwitchReasonNoCandidate  = "no_available_candidate"
	CodexQueueSwitchReasonAwaitingIdle = "awaiting_idle_window"
	CodexQueueSwitchReasonQuotaStale   = "quota_stale"

	codexQueueStaleSnapshotMultiplier = 2
)

// QuotaWindowSnapshot captures one Codex usage window.
type QuotaWindowSnapshot struct {
	PercentRemaining float64   `json:"percent_remaining"`
	WindowMinutes    int       `json:"window_minutes"`
	ResetAt          time.Time `json:"reset_at,omitempty"`
}

// CodexQuotaSnapshot holds the latest Codex usage information for one auth.
type CodexQuotaSnapshot struct {
	PrimaryWindow   QuotaWindowSnapshot `json:"primary_window"`
	SecondaryWindow QuotaWindowSnapshot `json:"secondary_window"`
	FetchedAt       time.Time           `json:"fetched_at,omitempty"`
	Source          string              `json:"source,omitempty"`
	Status          string              `json:"status,omitempty"`
	Error           string              `json:"error,omitempty"`
	Stale           bool                `json:"stale,omitempty"`
}

// CodexQueueAuthState describes one auth's queue runtime state.
type CodexQueueAuthState struct {
	AuthID               string             `json:"auth_id"`
	QueueGroup           string             `json:"queue_group"`
	QueuePosition        int                `json:"queue_position"`
	QueueState           string             `json:"queue_state"`
	QueueManagedDisabled bool               `json:"queue_managed_disabled"`
	QueueDisabledReason  string             `json:"queue_disabled_reason,omitempty"`
	RecoveryReadyAt      time.Time          `json:"recovery_ready_at,omitempty"`
	LastRealRequestAt    time.Time          `json:"last_real_request_at,omitempty"`
	SwitchPendingSince   time.Time          `json:"switch_pending_since,omitempty"`
	Quota                CodexQuotaSnapshot `json:"quota"`
}

// CodexQueueGroupState describes one equivalent Codex auth group.
type CodexQueueGroupState struct {
	GroupKey            string                `json:"group_key"`
	ActiveAuthID        string                `json:"active_auth_id,omitempty"`
	Members             []CodexQueueAuthState `json:"members"`
	SwitchPendingReason string                `json:"switch_pending_reason,omitempty"`
	UpdatedAt           time.Time             `json:"updated_at"`
}

// CodexQueueQuotaProvider is the interface the coordinator uses to fetch the
// latest Codex quota for an auth. Implementations should respect the repo's
// timeout-after-connect rule: they may use a short connect-timeout via the
// provided context, but must not enforce a deadline after the upstream
// connection is established.
type CodexQueueQuotaProvider interface {
	FetchCodexQuota(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error)
}

// CodexQueueQuotaProviderFunc adapts a function into CodexQueueQuotaProvider.
type CodexQueueQuotaProviderFunc func(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error)

// FetchCodexQuota implements CodexQueueQuotaProvider.
func (f CodexQueueQuotaProviderFunc) FetchCodexQuota(ctx context.Context, auth *Auth) (CodexQuotaSnapshot, error) {
	return f(ctx, auth)
}

// CodexQueueCoordinator owns Codex OAuth queue runtime state. It is created
// lazily when the queue mode is enabled in config and never mutates auth
// files or OAuth credential material.
type CodexQueueCoordinator struct {
	manager  *Manager
	provider CodexQueueQuotaProvider

	mu             sync.RWMutex
	cfg            internalconfig.CodexQueueConfig
	enabled        bool
	states         map[string]*CodexQueueAuthState
	groupMembers   map[string][]string // group key -> ordered auth IDs
	groupActive    map[string]string   // group key -> auth ID
	groupReason    map[string]string   // group key -> switch pending reason
	groupUpdatedAt map[string]time.Time

	// manualDisabled mirrors the manual operator-owned disabled status for
	// every Codex queue candidate seen during the last rebuildGroups pass.
	// It is captured from a cloned auth snapshot so coordinator code never
	// reaches back into the Manager auth map while holding only the
	// coordinator mutex.
	manualDisabled map[string]bool

	tickInterval        time.Duration
	activeRefreshEvery  time.Duration
	standbyRefreshEvery time.Duration

	stopCh chan struct{}
	wg     sync.WaitGroup

	now func() time.Time
}

// newCodexQueueCoordinator builds a coordinator bound to the given manager.
// The provider is optional; when nil, snapshots remain unknown.
func newCodexQueueCoordinator(manager *Manager, provider CodexQueueQuotaProvider) *CodexQueueCoordinator {
	c := &CodexQueueCoordinator{
		manager:             manager,
		provider:            provider,
		states:              make(map[string]*CodexQueueAuthState),
		groupMembers:        make(map[string][]string),
		groupActive:         make(map[string]string),
		groupReason:         make(map[string]string),
		groupUpdatedAt:      make(map[string]time.Time),
		manualDisabled:      make(map[string]bool),
		tickInterval:        30 * time.Second,
		activeRefreshEvery:  2 * time.Minute,
		standbyRefreshEvery: 10 * time.Minute,
		now:                 time.Now,
	}
	return c
}

// SetProvider swaps the quota provider implementation. Safe for runtime use.
func (c *CodexQueueCoordinator) SetProvider(provider CodexQueueQuotaProvider) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.provider = provider
	c.mu.Unlock()
}

// ApplyConfig refreshes the coordinator's configuration snapshot. It returns
// true when the enable flag transitioned.
func (c *CodexQueueCoordinator) ApplyConfig(cfg internalconfig.CodexQueueConfig) bool {
	if c == nil {
		return false
	}
	cfg.Normalize()
	c.mu.Lock()
	previous := c.enabled
	c.cfg = cfg
	c.enabled = cfg.Enabled
	transitioned := previous != c.enabled
	c.mu.Unlock()
	return transitioned
}

// Enabled reports whether the coordinator is currently active.
func (c *CodexQueueCoordinator) Enabled() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// Config returns a copy of the active configuration.
func (c *CodexQueueCoordinator) Config() internalconfig.CodexQueueConfig {
	if c == nil {
		return internalconfig.CodexQueueConfig{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	cfgCopy := c.cfg
	if c.cfg.AutoDisableCurrent != nil {
		v := *c.cfg.AutoDisableCurrent
		cfgCopy.AutoDisableCurrent = &v
	}
	if len(c.cfg.GroupBy) > 0 {
		out := make([]string, len(c.cfg.GroupBy))
		copy(out, c.cfg.GroupBy)
		cfgCopy.GroupBy = out
	}
	return cfgCopy
}

// Start begins the background refresh loop. It is idempotent.
func (c *CodexQueueCoordinator) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopCh != nil {
		c.mu.Unlock()
		return
	}
	c.stopCh = make(chan struct{})
	stop := c.stopCh
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runLoop(ctx, stop)
	}()
}

// Stop terminates the background loop and clears runtime queue-managed
// disabled state so previously gated auths can return to scheduler routing.
func (c *CodexQueueCoordinator) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	stop := c.stopCh
	c.stopCh = nil
	// Disabling here ensures isQueueRoutingBlocked stops blocking standby
	// members during the upcoming scheduler refresh. Without this, the
	// scheduler upserts in clearAllManagedDisabled would still remove the
	// auths because the checker would observe enabled=true.
	c.enabled = false
	c.mu.Unlock()
	if stop != nil {
		close(stop)
		c.wg.Wait()
	}
	c.clearAllManagedDisabled(true)
}

func (c *CodexQueueCoordinator) runLoop(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(c.tickInterval)
	defer ticker.Stop()
	// Run one immediate tick when enabled so promotion happens without waiting
	// the full interval. The Reconcile call is safe even when the coordinator
	// has not been seeded with auth data yet.
	c.Reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			c.Reconcile(ctx)
		}
	}
}

// RecordRealRequest is invoked by the conductor for every non-synthetic
// completion. It updates the active auth's idle timer and resets the
// switch-pending timer when applicable.
func (c *CodexQueueCoordinator) RecordRealRequest(authID string, t time.Time) {
	if c == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return
	}
	state := c.states[authID]
	if state == nil {
		return
	}
	if t.IsZero() {
		t = c.now()
	}
	state.LastRealRequestAt = t
	if state.QueueState == CodexQueueStateSwitchPending {
		state.SwitchPendingSince = t
	}
}

// Reconcile recomputes group membership and queue state from the latest auth
// snapshot. It is safe to call from any goroutine.
func (c *CodexQueueCoordinator) Reconcile(ctx context.Context) {
	if c == nil {
		return
	}
	if !c.Enabled() {
		return
	}
	auths := c.snapshotAuths()
	cfg := c.Config()
	c.rebuildGroups(auths, cfg)
	c.refreshQuotas(ctx, auths, cfg)
	dirty := c.evaluateState(cfg)
	c.pushSchedulerUpdatesForAuthIDs(dirty)
}

// ReconcileNow forces a synchronous reconcile pass. It exists so callers that
// have just enabled queue mode (config reload, management toggle, startup)
// can ensure the scheduler is updated before serving any traffic.
func (c *CodexQueueCoordinator) ReconcileNow(ctx context.Context) {
	c.Reconcile(ctx)
}

func (c *CodexQueueCoordinator) snapshotAuths() []*Auth {
	if c == nil || c.manager == nil {
		return nil
	}
	return c.manager.snapshotAuths()
}

func (c *CodexQueueCoordinator) rebuildGroups(auths []*Auth, cfg internalconfig.CodexQueueConfig) {
	groupMembers := make(map[string][]string)
	authGroup := make(map[string]string)
	manualDisabled := make(map[string]bool)
	keys := cfg.EffectiveGroupBy()

	for _, a := range auths {
		if !isCodexOAuthCandidate(a) {
			continue
		}
		key := codexGroupKey(a, keys)
		if key == "" {
			continue
		}
		groupMembers[key] = append(groupMembers[key], a.ID)
		authGroup[a.ID] = key
		manualDisabled[a.ID] = a.Disabled || a.Status == StatusDisabled
	}

	for k := range groupMembers {
		sort.Strings(groupMembers[k])
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Prune state entries for auths that are no longer Codex queue candidates.
	for id := range c.states {
		if _, ok := authGroup[id]; !ok {
			delete(c.states, id)
		}
	}

	c.groupMembers = groupMembers
	c.manualDisabled = manualDisabled
	// Recompute group active mapping; preserve the previously elected active
	// only when it is still present in the group AND is not manually disabled.
	// A manually disabled active must not block the coordinator from
	// promoting the next candidate during evaluateState.
	groupActive := make(map[string]string, len(groupMembers))
	for k, members := range groupMembers {
		prev := c.groupActive[k]
		if prev == "" {
			continue
		}
		if manualDisabled[prev] {
			continue
		}
		for _, id := range members {
			if id == prev {
				groupActive[k] = prev
				break
			}
		}
	}
	c.groupActive = groupActive

	// Ensure state entries exist for current members.
	for authID, groupKey := range authGroup {
		state, ok := c.states[authID]
		if !ok || state == nil {
			state = &CodexQueueAuthState{AuthID: authID}
			c.states[authID] = state
		}
		state.QueueGroup = groupKey
	}

	// Refresh per-state position/queue state. The detailed promotion logic
	// runs in evaluateState, which we call after rebuildGroups/refreshQuotas.
	for k, members := range groupMembers {
		for idx, authID := range members {
			state := c.states[authID]
			if state == nil {
				continue
			}
			state.QueuePosition = idx + 1
			if len(members) < 2 {
				state.QueueState = CodexQueueStateIneligibleGroup
				state.QueueManagedDisabled = false
				state.QueueDisabledReason = ""
				state.RecoveryReadyAt = time.Time{}
				continue
			}
			if manualDisabled[authID] {
				state.QueueState = CodexQueueStateManualDisabled
				// Manual disable always wins. Clear queue-managed flags so
				// the operator-owned disable status is not masked.
				state.QueueManagedDisabled = false
				state.QueueDisabledReason = ""
				state.RecoveryReadyAt = time.Time{}
				continue
			}
			if c.groupActive[k] == authID {
				state.RecoveryReadyAt = time.Time{}
				state.QueueState = CodexQueueStateActive
				continue
			}
			if state.QueueManagedDisabled {
				state.QueueState = CodexQueueStateManagedDisabled
				continue
			}
			// Default non-active members to standby. evaluateState refines
			// the label to quota_unknown/quota_error after quota refresh
			// when applicable.
			state.QueueState = CodexQueueStateStandby
		}
	}
}

func (c *CodexQueueCoordinator) refreshQuotas(ctx context.Context, auths []*Auth, cfg internalconfig.CodexQueueConfig) {
	c.mu.RLock()
	provider := c.provider
	c.mu.RUnlock()
	if provider == nil {
		return
	}
	idleWindow := cfg.IdleWindowDuration()
	staleAfter := idleWindow
	if staleAfter < c.activeRefreshEvery*codexQueueStaleSnapshotMultiplier {
		staleAfter = c.activeRefreshEvery * codexQueueStaleSnapshotMultiplier
	}

	now := c.now()
	type fetchPlan struct {
		auth *Auth
	}
	plans := make([]fetchPlan, 0, len(auths))

	c.mu.RLock()
	for _, a := range auths {
		if a == nil {
			continue
		}
		state, ok := c.states[a.ID]
		if !ok || state == nil {
			continue
		}
		isActive := c.groupActive[state.QueueGroup] == a.ID
		interval := c.standbyRefreshEvery
		if isActive {
			interval = c.activeRefreshEvery
		}
		if !state.Quota.FetchedAt.IsZero() && now.Sub(state.Quota.FetchedAt) < interval && state.Quota.Status != "" {
			continue
		}
		plans = append(plans, fetchPlan{auth: a})
	}
	c.mu.RUnlock()

	for _, plan := range plans {
		snapshot, err := provider.FetchCodexQuota(ctx, plan.auth)
		c.mu.Lock()
		state := c.states[plan.auth.ID]
		if state == nil {
			c.mu.Unlock()
			continue
		}
		if err != nil {
			state.Quota = CodexQuotaSnapshot{
				FetchedAt: now,
				Source:    snapshot.Source,
				Status:    CodexQuotaStatusError,
				Error:     redactQuotaError(err),
			}
		} else {
			snapshot.FetchedAt = now
			if snapshot.Status == "" {
				snapshot.Status = CodexQuotaStatusKnown
			}
			snapshot.Stale = false
			state.Quota = snapshot
		}
		c.mu.Unlock()
	}

	// Mark stale snapshots so they remain visible in management but cannot
	// satisfy the promotion threshold check.
	c.mu.Lock()
	for _, state := range c.states {
		if state == nil {
			continue
		}
		if state.Quota.Status != CodexQuotaStatusKnown {
			continue
		}
		if !state.Quota.FetchedAt.IsZero() && now.Sub(state.Quota.FetchedAt) > staleAfter {
			state.Quota.Stale = true
		}
	}
	c.mu.Unlock()
}

// evaluateState applies promotion / manual-disable / quota-derived state
// transitions. It returns the set of auth IDs whose scheduler-visible routing
// state may have changed during this pass so callers can issue scheduler
// upserts/removes.
func (c *CodexQueueCoordinator) evaluateState(cfg internalconfig.CodexQueueConfig) []string {
	now := c.now()
	idleWindow := cfg.IdleWindowDuration()
	threshold := cfg.ThresholdPercent
	autoDisable := cfg.AutoDisableCurrentEnabled()

	type promotion struct {
		groupKey string
		from     string
		to       string
		reason   string
	}
	var promotions []promotion
	dirty := make(map[string]struct{})

	c.mu.Lock()
	for groupKey, members := range c.groupMembers {
		if len(members) < 2 {
			c.groupReason[groupKey] = ""
			c.groupUpdatedAt[groupKey] = now
			for _, id := range members {
				dirty[id] = struct{}{}
			}
			continue
		}
		// Every member of an eligible queue group is routing-sensitive on
		// every pass: scheduler eligibility flips depending on whether the
		// member is the elected active or not.
		for _, id := range members {
			dirty[id] = struct{}{}
		}

		// Surface quota-derived state for standby members before any
		// promotion decision so management/auth-files responses match the
		// design's state model.
		for _, id := range members {
			state := c.states[id]
			if state == nil {
				continue
			}
			if c.manualDisabled[id] {
				state.QueueState = CodexQueueStateManualDisabled
				state.QueueManagedDisabled = false
				state.QueueDisabledReason = ""
				state.RecoveryReadyAt = time.Time{}
				continue
			}
			if c.groupActive[groupKey] == id {
				continue
			}
			if state.QueueManagedDisabled {
				if queueRecoveryEligible(state, threshold, c.manualDisabled[id]) {
					state.QueueManagedDisabled = false
					state.QueueDisabledReason = ""
					state.QueueState = CodexQueueStateStandby
					state.RecoveryReadyAt = now.Add(cfg.RecoveryDwellDuration())
					continue
				}
				state.QueueState = CodexQueueStateManagedDisabled
				continue
			}
			if !state.RecoveryReadyAt.IsZero() && !now.Before(state.RecoveryReadyAt) {
				state.RecoveryReadyAt = time.Time{}
			}
			switch state.Quota.Status {
			case CodexQuotaStatusError:
				state.QueueState = CodexQueueStateQuotaError
			case CodexQuotaStatusUnknown, "":
				state.QueueState = CodexQueueStateQuotaUnknown
			default:
				state.QueueState = CodexQueueStateStandby
			}
		}

		activeID := c.groupActive[groupKey]
		if activeID == "" {
			candidate := c.firstPromotableLocked(groupKey, "", cfg, true)
			if candidate != "" {
				c.groupActive[groupKey] = candidate
				activeID = candidate
				if state := c.states[candidate]; state != nil {
					state.QueueManagedDisabled = false
					state.QueueDisabledReason = ""
					state.RecoveryReadyAt = time.Time{}
					state.QueueState = CodexQueueStateActive
					if state.LastRealRequestAt.IsZero() {
						state.LastRealRequestAt = now
					}
				}
			}
		}

		activeState := c.states[activeID]
		if activeState == nil {
			c.groupReason[groupKey] = CodexQueueSwitchReasonNoCandidate
			c.groupUpdatedAt[groupKey] = now
			continue
		}

		lowQuota := isLowQuota(activeState.Quota, threshold)
		if lowQuota {
			if activeState.QueueState != CodexQueueStateSwitchPending {
				activeState.QueueState = CodexQueueStateSwitchPending
				if activeState.SwitchPendingSince.IsZero() {
					activeState.SwitchPendingSince = now
				}
			}
			c.groupReason[groupKey] = CodexQueueSwitchReasonLowQuota
		} else {
			// Quota recovered; back to active.
			if activeState.QueueState == CodexQueueStateSwitchPending {
				activeState.QueueState = CodexQueueStateActive
				activeState.SwitchPendingSince = time.Time{}
			}
			c.groupReason[groupKey] = ""
		}

		// Promotion gate: low quota + idle window elapsed + candidate exists.
		if !lowQuota {
			c.groupUpdatedAt[groupKey] = now
			continue
		}
		idleElapsed := false
		if activeState.LastRealRequestAt.IsZero() {
			// Treat zero last-request as "active just registered"; require a
			// full idle window before switching to avoid early flapping.
			idleElapsed = now.Sub(activeState.SwitchPendingSince) >= idleWindow
		} else {
			idleElapsed = now.Sub(activeState.LastRealRequestAt) >= idleWindow
		}
		if !idleElapsed {
			c.groupReason[groupKey] = CodexQueueSwitchReasonAwaitingIdle
			c.groupUpdatedAt[groupKey] = now
			continue
		}

		candidate := c.firstPromotableLocked(groupKey, activeID, cfg, false)
		if candidate == "" {
			c.groupReason[groupKey] = CodexQueueSwitchReasonNoCandidate
			c.groupUpdatedAt[groupKey] = now
			continue
		}

		if autoDisable {
			activeState.QueueManagedDisabled = true
			activeState.QueueDisabledReason = CodexQueueDisableReasonLowQuota
			activeState.RecoveryReadyAt = time.Time{}
			activeState.QueueState = CodexQueueStateManagedDisabled
		}
		if next := c.states[candidate]; next != nil {
			next.QueueManagedDisabled = false
			next.QueueDisabledReason = ""
			next.RecoveryReadyAt = time.Time{}
			next.QueueState = CodexQueueStateActive
			next.SwitchPendingSince = time.Time{}
			next.LastRealRequestAt = now
		}
		c.groupActive[groupKey] = candidate
		c.groupReason[groupKey] = ""
		c.groupUpdatedAt[groupKey] = now
		promotions = append(promotions, promotion{
			groupKey: groupKey,
			from:     activeID,
			to:       candidate,
			reason:   CodexQueueDisableReasonLowQuota,
		})
	}
	// Clean up state entries for non-existent groups.
	for groupKey := range c.groupReason {
		if _, ok := c.groupMembers[groupKey]; !ok {
			delete(c.groupReason, groupKey)
			delete(c.groupActive, groupKey)
			delete(c.groupUpdatedAt, groupKey)
		}
	}
	c.mu.Unlock()

	for _, p := range promotions {
		log.WithFields(log.Fields{
			"group":  p.groupKey,
			"from":   p.from,
			"to":     p.to,
			"reason": p.reason,
		}).Info("codex queue mode: switched active auth")
	}

	out := make([]string, 0, len(dirty))
	for id := range dirty {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// firstPromotableLocked returns the first member of groupKey that is eligible
// to be the active auth. The permissive flag relaxes the unknown-quota gate
// when there is no current active to fall back to; this prevents the queue
// from leaving all routing blocked while quota is still being learned for
// the first time. Manual disabled state is read from the snapshot captured
// during rebuildGroups so this method can run without re-entering the
// Manager lock.
func (c *CodexQueueCoordinator) firstPromotableLocked(groupKey, excludeID string, cfg internalconfig.CodexQueueConfig, permissive bool) string {
	members := c.groupMembers[groupKey]
	threshold := cfg.ThresholdPercent
	unknownPolicy := strings.ToLower(strings.TrimSpace(cfg.UnknownQuotaPolicy))
	now := c.now()
	for _, id := range members {
		if id == excludeID {
			continue
		}
		state := c.states[id]
		if state == nil {
			continue
		}
		if state.QueueManagedDisabled {
			continue
		}
		if !state.RecoveryReadyAt.IsZero() && now.Before(state.RecoveryReadyAt) {
			continue
		}
		// Manual disabled wins; never promote.
		if c.manualDisabled[id] {
			continue
		}
		switch state.Quota.Status {
		case CodexQuotaStatusKnown:
			if state.Quota.Stale {
				continue
			}
			if isLowQuota(state.Quota, threshold) {
				continue
			}
			return id
		case CodexQuotaStatusError, CodexQuotaStatusUnknown, "":
			if permissive || unknownPolicy == internalconfig.CodexQueueUnknownQuotaPolicyPromote {
				return id
			}
		}
	}
	return ""
}

// clearAllManagedDisabled clears queue-managed disabled flags and switch
// pending timers. When the coordinator is being torn down (queueWasEnabled
// is true), it also re-syncs every previously known queue member with the
// scheduler so standby/managed-disabled auths return to ready routing.
func (c *CodexQueueCoordinator) clearAllManagedDisabled(queueWasEnabled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	toRefresh := make([]string, 0, len(c.states))
	for id, state := range c.states {
		if state == nil {
			continue
		}
		previouslyBlocked := state.QueueManagedDisabled
		if state.QueueManagedDisabled {
			state.QueueManagedDisabled = false
			state.QueueDisabledReason = ""
		}
		state.RecoveryReadyAt = time.Time{}
		state.SwitchPendingSince = time.Time{}
		// When queue mode is being disabled, re-register every member so
		// the scheduler stops treating non-active group members as
		// queue-blocked. Without this, healthy standby auths would remain
		// invisible to routing until something else upserts them.
		if previouslyBlocked || queueWasEnabled {
			toRefresh = append(toRefresh, id)
		}
	}
	c.mu.Unlock()
	for _, id := range toRefresh {
		c.pushSchedulerUpdate(id)
	}
}

// ResetGroup clears the queue-managed disabled state for a single group key.
// Returns the number of auths whose state was modified. Manual disabled state
// is left intact.
func (c *CodexQueueCoordinator) ResetGroup(groupKey string) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	members := c.groupMembers[groupKey]
	refresh := make([]string, 0, len(members))
	for _, id := range members {
		state := c.states[id]
		if state == nil {
			continue
		}
		if state.QueueManagedDisabled {
			state.QueueManagedDisabled = false
			state.QueueDisabledReason = ""
			state.RecoveryReadyAt = time.Time{}
			state.QueueState = CodexQueueStateStandby
			refresh = append(refresh, id)
		}
		state.SwitchPendingSince = time.Time{}
	}
	delete(c.groupActive, groupKey)
	c.mu.Unlock()
	for _, id := range refresh {
		c.pushSchedulerUpdate(id)
	}
	return len(refresh)
}

// ResetAuth clears the queue-managed disabled state for a single auth.
// Returns true when the state was modified.
func (c *CodexQueueCoordinator) ResetAuth(authID string) bool {
	if c == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	c.mu.Lock()
	state := c.states[authID]
	if state == nil {
		c.mu.Unlock()
		return false
	}
	if !state.QueueManagedDisabled {
		c.mu.Unlock()
		return false
	}
	state.QueueManagedDisabled = false
	state.QueueDisabledReason = ""
	state.RecoveryReadyAt = time.Time{}
	state.QueueState = CodexQueueStateStandby
	state.SwitchPendingSince = time.Time{}
	c.mu.Unlock()
	c.pushSchedulerUpdate(authID)
	return true
}

// IsQueueManagedDisabled returns true when the auth has been disabled by the
// coordinator. It is safe to call without holding any external lock.
func (c *CodexQueueCoordinator) IsQueueManagedDisabled(authID string) bool {
	if c == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	state := c.states[authID]
	if state == nil {
		return false
	}
	return state.QueueManagedDisabled
}

// IsQueueRoutingBlocked reports whether the given auth must be excluded from
// scheduler routing because queue mode is enforcing "one active at a time"
// for its group. Auths that are not part of any eligible queue group (e.g.
// singletons or non-Codex auths) are not affected; the scheduler keeps its
// existing behavior for them.
//
// The check returns true when any of the following holds:
//   - the auth has been explicitly marked queue-managed disabled (auto switch
//     after low quota plus idle window);
//   - the auth is a member of an eligible queue group (>=2 members) and is
//     not the currently elected active auth for the group, including the
//     transient case where the coordinator has not yet elected one.
func (c *CodexQueueCoordinator) IsQueueRoutingBlocked(authID string) bool {
	if c == nil {
		return false
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.enabled {
		return false
	}
	state := c.states[authID]
	if state == nil {
		return false
	}
	if state.QueueManagedDisabled {
		return true
	}
	groupKey := state.QueueGroup
	if groupKey == "" {
		return false
	}
	members := c.groupMembers[groupKey]
	if len(members) < 2 {
		return false
	}
	active := c.groupActive[groupKey]
	if active == "" {
		// During the transient window between rebuildGroups and
		// firstPromotableLocked picking an active, block every member
		// except a deterministic fallback so requests still find one
		// auth. The fallback is the first member alphabetically; this is
		// stable across processes and matches the order used by
		// firstPromotableLocked.
		if len(members) > 0 && members[0] == authID {
			return false
		}
		return true
	}
	return active != authID
}

// AuthState returns a copy of the runtime state for the given auth, or nil.
func (c *CodexQueueCoordinator) AuthState(authID string) *CodexQueueAuthState {
	if c == nil {
		return nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	state := c.states[authID]
	if state == nil {
		return nil
	}
	cp := *state
	return &cp
}

// Groups returns a deterministic snapshot of every known queue group.
func (c *CodexQueueCoordinator) Groups() []CodexQueueGroupState {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.groupMembers))
	for k := range c.groupMembers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]CodexQueueGroupState, 0, len(keys))
	for _, k := range keys {
		members := c.groupMembers[k]
		entry := CodexQueueGroupState{
			GroupKey:            k,
			ActiveAuthID:        c.groupActive[k],
			SwitchPendingReason: c.groupReason[k],
			UpdatedAt:           c.groupUpdatedAt[k],
		}
		entry.Members = make([]CodexQueueAuthState, 0, len(members))
		for _, id := range members {
			state := c.states[id]
			if state == nil {
				continue
			}
			entry.Members = append(entry.Members, *state)
		}
		out = append(out, entry)
	}
	return out
}

func (c *CodexQueueCoordinator) pushSchedulerUpdate(authID string) {
	if c == nil || c.manager == nil || c.manager.scheduler == nil {
		return
	}
	auth, ok := c.manager.GetByID(authID)
	if !ok || auth == nil {
		return
	}
	c.manager.scheduler.upsertAuth(auth)
}

// pushSchedulerUpdatesForAuthIDs refreshes the scheduler entry for each
// supplied auth ID. The scheduler's upsertAuthLocked consults the
// queue-routing-blocked checker registered by the coordinator, so this is
// how queue mode forces standby/managed-disabled members out of the ready
// path. Must NOT be called while holding the coordinator mutex; the
// scheduler upsert path may re-enter the package-level checker.
func (c *CodexQueueCoordinator) pushSchedulerUpdatesForAuthIDs(ids []string) {
	if c == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		c.pushSchedulerUpdate(id)
	}
}

func queueRecoveryEligible(state *CodexQueueAuthState, threshold float64, manualDisabled bool) bool {
	if state == nil {
		return false
	}
	return state.Quota.Status == CodexQuotaStatusKnown &&
		!state.Quota.Stale &&
		!isLowQuota(state.Quota, threshold) &&
		!manualDisabled
}

// isLowQuota reports whether any known window's percent-remaining falls below
// threshold.
func isLowQuota(snapshot CodexQuotaSnapshot, threshold float64) bool {
	if snapshot.Status != CodexQuotaStatusKnown {
		return false
	}
	if snapshot.Stale {
		return false
	}
	if windowKnown(snapshot.PrimaryWindow) && snapshot.PrimaryWindow.PercentRemaining < threshold {
		return true
	}
	if windowKnown(snapshot.SecondaryWindow) && snapshot.SecondaryWindow.PercentRemaining < threshold {
		return true
	}
	return false
}

func windowKnown(w QuotaWindowSnapshot) bool {
	return w.PercentRemaining >= 0 && (w.WindowMinutes > 0 || !w.ResetAt.IsZero() || w.PercentRemaining > 0)
}

func isCodexOAuthCandidate(a *Auth) bool {
	if a == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		return false
	}
	// Skip pure API-key (codex-api-key) entries; queue mode targets file-backed
	// OAuth auths only.
	if a.Attributes != nil {
		if key := strings.TrimSpace(a.Attributes["api_key"]); key != "" {
			return false
		}
		if strings.EqualFold(strings.TrimSpace(a.Attributes["runtime_only"]), "true") {
			return false
		}
	}
	// First release only admits paid non-credits plans.
	plan := strings.ToLower(strings.TrimSpace(codexPlanType(a)))
	switch plan {
	case "free", "credits":
		return false
	}
	return true
}

// codexPlanType extracts the chatgpt_plan_type either from Attributes or the
// embedded id_token JWT.
func codexPlanType(a *Auth) string {
	if a == nil {
		return ""
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["plan_type"]); v != "" {
			return v
		}
	}
	if a.Metadata == nil {
		return ""
	}
	if raw, ok := a.Metadata["id_token"].(string); ok {
		if claims, err := codexjwt.ParseJWTToken(raw); err == nil && claims != nil {
			if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
				return v
			}
		}
	}
	return ""
}

// codexAccountID extracts a chatgpt account ID for the auth.
func codexAccountID(a *Auth) string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["account_id"].(string); ok {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				return trimmed
			}
		}
		if raw, ok := a.Metadata["id_token"].(string); ok {
			if claims, err := codexjwt.ParseJWTToken(raw); err == nil && claims != nil {
				if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// codexAccessToken returns the bearer access token for the auth, if any.
func codexAccessToken(a *Auth) string {
	if a == nil || a.Metadata == nil {
		return ""
	}
	if v, ok := a.Metadata["access_token"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// codexGroupKey returns a stable hash describing the equivalence group for one
// Codex auth. The set of components included is taken from the configured
// group_by list. The hash is opaque to callers.
func codexGroupKey(a *Auth, components []string) string {
	if a == nil {
		return ""
	}
	if len(components) == 0 {
		components = (internalconfig.CodexQueueConfig{}).EffectiveGroupBy()
	}
	type kv struct {
		Key   string
		Value string
	}
	parts := []kv{{Key: "provider", Value: "codex"}}
	for _, c := range components {
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "prefix":
			parts = append(parts, kv{Key: "prefix", Value: strings.TrimSpace(a.Prefix)})
		case "models":
			set := supportedModelSetForAuth(a.ID)
			models := make([]string, 0, len(set))
			for m := range set {
				models = append(models, m)
			}
			sort.Strings(models)
			parts = append(parts, kv{Key: "models", Value: strings.Join(models, "\x00")})
		case "websockets":
			parts = append(parts, kv{Key: "websockets", Value: boolKey(authWebsocketsEnabled(a))})
		case "headers":
			parts = append(parts, kv{Key: "headers", Value: stableHeaderKey(a)})
		case "plan_type":
			parts = append(parts, kv{Key: "plan_type", Value: strings.ToLower(strings.TrimSpace(codexPlanType(a)))})
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].Key < parts[j].Key })
	encoded, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:12])
}

func boolKey(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// stableHeaderKey serializes the routing-relevant attributes (`inject_headers`,
// `default_model`, `base_url`) into a deterministic JSON string. Missing keys
// are normalized to "" to ensure identical group keys across processes.
func stableHeaderKey(a *Auth) string {
	if a == nil {
		return ""
	}
	type entry struct {
		Key   string
		Value string
	}
	picks := []entry{
		{Key: "inject_headers"},
		{Key: "default_model"},
		{Key: "base_url"},
	}
	for i := range picks {
		if a.Attributes != nil {
			picks[i].Value = strings.TrimSpace(a.Attributes[picks[i].Key])
		}
	}
	encoded, err := json.Marshal(picks)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func redactQuotaError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Strip embedded bearer tokens, accounts and explicit URL queries.
	lower := strings.ToLower(msg)
	if idx := strings.Index(lower, "bearer "); idx >= 0 {
		msg = msg[:idx] + "Bearer [REDACTED]"
	}
	if idx := strings.Index(msg, "chatgpt-account-id"); idx >= 0 {
		msg = msg[:idx] + "chatgpt-account-id=[REDACTED]"
	}
	return msg
}

// Package-level queue managed disabled checker. Scheduler and selector use it
// to keep their existing function signatures intact.
var queueManagedDisabledChecker atomic.Pointer[func(string) bool]

// SetQueueManagedDisabledChecker registers a checker that selector and
// scheduler consult when evaluating whether an auth participates in routing.
// Passing nil clears the checker.
func SetQueueManagedDisabledChecker(fn func(string) bool) {
	if fn == nil {
		queueManagedDisabledChecker.Store(nil)
		return
	}
	queueManagedDisabledChecker.Store(&fn)
}

// isQueueManagedDisabled is the safe accessor for the checker.
func isQueueManagedDisabled(authID string) bool {
	ptr := queueManagedDisabledChecker.Load()
	if ptr == nil || *ptr == nil {
		return false
	}
	return (*ptr)(authID)
}

// Package-level queue routing block checker. Scheduler and selector consult
// it to enforce "one active auth per group at a time" when queue mode is
// enabled. The check is broader than queueManagedDisabledChecker because it
// also blocks healthy standby members.
var queueRoutingBlockedChecker atomic.Pointer[func(string) bool]

// SetQueueRoutingBlockedChecker registers the routing block checker. Passing
// nil clears it.
func SetQueueRoutingBlockedChecker(fn func(string) bool) {
	if fn == nil {
		queueRoutingBlockedChecker.Store(nil)
		return
	}
	queueRoutingBlockedChecker.Store(&fn)
}

// isQueueRoutingBlocked is the safe accessor for the routing-block checker.
func isQueueRoutingBlocked(authID string) bool {
	ptr := queueRoutingBlockedChecker.Load()
	if ptr == nil || *ptr == nil {
		return false
	}
	return (*ptr)(authID)
}

// Package-level real-request recorder used by the conductor.
var queueRealRequestRecorder atomic.Pointer[func(string, time.Time)]

// SetQueueRealRequestRecorder registers a callback invoked for every
// non-synthetic completion. Passing nil clears the recorder.
func SetQueueRealRequestRecorder(fn func(string, time.Time)) {
	if fn == nil {
		queueRealRequestRecorder.Store(nil)
		return
	}
	queueRealRequestRecorder.Store(&fn)
}

// recordRealRequestForQueue is the safe accessor for the recorder.
func recordRealRequestForQueue(authID string, t time.Time) {
	ptr := queueRealRequestRecorder.Load()
	if ptr == nil || *ptr == nil {
		return
	}
	(*ptr)(authID, t)
}

// ensure unused imports are referenced for future expansion if needed.
var _ = errors.New
