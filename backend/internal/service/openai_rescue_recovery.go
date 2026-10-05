package service

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"sort"
	"time"
)

var ErrRescueRecoveryUnverified = errors.New("rescue recovery requires current plugin and host qualification evidence")

const openAIRescueEvidenceWindow = 15 * time.Minute

func isOpenAIRescueAccountActive(account *Account) bool {
	marker := GetOpenAIRescueLaneMarker(account)
	return marker != nil && marker.ExitReason == "" && !OpenAIRescueManuallyTerminated(account)
}

type OpenAIRescueGraduation struct {
	AccountID         int64
	ExpectedUpdatedAt time.Time
	HostProbeID       int64
	HostProbeAt       time.Time
	EvidenceExpiresAt time.Time
	OldGroupIDs       []int64
	GroupIDs          []int64
	Extra             map[string]any
}

type OpenAIRescueGraduationStore interface {
	CommitOpenAIRescueGraduation(context.Context, OpenAIRescueGraduation) error
}

type OpenAIRescueMarkerUpdate struct {
	AccountID         int64
	ExpectedUpdatedAt time.Time
	Marker            OpenAIRescueLaneMarker
	RawMarker         map[string]any
	Withdraw          bool
}

type OpenAIRescueMarkerStore interface {
	CommitOpenAIRescueMarker(context.Context, OpenAIRescueMarkerUpdate) (time.Time, error)
}

type OpenAIRescueTransition string

const (
	OpenAIRescueTransitionEnter  OpenAIRescueTransition = "enter"
	OpenAIRescueTransitionExit   OpenAIRescueTransition = "exit"
	OpenAIRescueTransitionRebind OpenAIRescueTransition = "rebind"
)

type OpenAIRescueTransitionUpdate struct {
	AccountID         int64
	ExpectedUpdatedAt time.Time
	Kind              OpenAIRescueTransition
	OldGroupIDs       []int64
	GroupIDs          []int64
	Extra             map[string]any
	ExpectedMarker    any
}

type OpenAIRescueTransitionStore interface {
	CommitOpenAIRescueTransition(context.Context, OpenAIRescueTransitionUpdate) (time.Time, error)
}

func (l *OpenAIRescueLane) commitTransition(ctx context.Context, account *Account, kind OpenAIRescueTransition,
	groupIDs []int64, extra map[string]any,
) error {
	store, ok := l.accounts.(OpenAIRescueTransitionStore)
	if !ok {
		return ErrOpenAIProbeAtomicStore
	}
	updatedAt, err := store.CommitOpenAIRescueTransition(ctx, OpenAIRescueTransitionUpdate{
		AccountID: account.ID, ExpectedUpdatedAt: account.UpdatedAt, Kind: kind,
		OldGroupIDs: account.GroupIDs, GroupIDs: groupIDs, Extra: extra,
		ExpectedMarker: account.Extra[openAIRescueLaneExtraKey],
	})
	if err != nil {
		return err
	}
	account.UpdatedAt, account.GroupIDs = updatedAt, groupIDs
	account.Schedulable = false
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	for key, value := range extra {
		account.Extra[key] = value
	}
	return nil
}

func (l *OpenAIRescueLane) commitMarker(ctx context.Context, account *Account, marker *OpenAIRescueLaneMarker, withdraw bool) error {
	store, ok := l.accounts.(OpenAIRescueMarkerStore)
	if !ok {
		return ErrOpenAIProbeAtomicStore
	}
	current := GetOpenAIRescueLaneMarker(account)
	if current == nil || !current.EnteredAt.Equal(marker.EnteredAt) || OpenAIRescueManuallyTerminated(account) {
		return ErrOpenAIProbeStale
	}
	fields := maps.Clone(account.Extra[openAIRescueLaneExtraKey].(map[string]any))
	before, after := rescueLaneMarkerExtraValue(*current), rescueLaneMarkerExtraValue(*marker)
	for key := range before {
		if _, present := after[key]; !present {
			delete(fields, key)
		}
	}
	for key, value := range after {
		if !reflect.DeepEqual(before[key], value) {
			fields[key] = value
		}
	}
	updatedAt, err := store.CommitOpenAIRescueMarker(ctx, OpenAIRescueMarkerUpdate{
		AccountID: account.ID, ExpectedUpdatedAt: account.UpdatedAt, Marker: *marker, RawMarker: fields, Withdraw: withdraw,
	})
	if err != nil {
		return err
	}
	account.UpdatedAt = updatedAt
	account.Extra[openAIRescueLaneExtraKey] = fields
	if withdraw {
		account.Schedulable = false
		account.Extra[openAIRescueSuspectedExtraKey] = true
	}
	return nil
}

// Observation survives plugin/host restarts but contains no request templates
// or credentials. Losing a template is an operational event, not recovery.
type openAIRescueObservation struct {
	SuspectedAt     time.Time   `json:"suspected_at,omitempty"`
	PluginSeen      bool        `json:"plugin_seen,omitempty"`
	StateLost       bool        `json:"state_lost,omitempty"`
	StateLosses     []time.Time `json:"state_losses,omitempty"`
	BackoffObserved bool        `json:"backoff_observed,omitempty"`
}

func (l *OpenAIRescueLane) recoveryBridge(ctx context.Context) *OpenAIPluginBridgeProber {
	if l == nil || l.bridge == nil {
		return nil
	}
	status := l.bridge(ctx)
	if status == nil || !status.Running || !status.Healthy || status.Offline {
		return nil
	}
	prober := ParseOpenAIPluginBridgeProber(status.StatusJSON)
	if prober == nil || !prober.Enabled {
		return nil
	}
	return prober
}

func rescueEvidenceStart(marker *OpenAIRescueLaneMarker) time.Time {
	start := marker.EnteredAt
	if observation := marker.Observation; observation != nil {
		if observation.SuspectedAt.After(start) {
			start = observation.SuspectedAt
		}
		for _, lost := range observation.StateLosses {
			if lost.After(start) {
				start = lost
			}
		}
	}
	return start
}

func rescuePluginPassEvidence(plugin *OpenAIPluginBridgeAccount, marker *OpenAIRescueLaneMarker, now time.Time) bool {
	if marker == nil || marker.ExitReason != "" || plugin == nil ||
		plugin.InBackoff || plugin.SuspectAccountLevel || plugin.ConsecFails != 0 ||
		plugin.ConsecutivePasses < 2 || plugin.LastVerdict != "pass" {
		return false
	}
	at := plugin.LastProbeAt
	previous := plugin.PreviousPassAt
	return !at.IsZero() && !previous.IsZero() &&
		!previous.Before(rescueEvidenceStart(marker)) && !previous.After(at) &&
		now.Sub(previous) <= openAIRescueEvidenceWindow &&
		!at.After(now) && now.Sub(at) <= openAIRescueEvidenceWindow
}

func rescueLaneRecoveryEvidence(plugin *OpenAIPluginBridgeAccount, host OpenAIProbeHealthSnapshot,
	marker *OpenAIRescueLaneMarker, now time.Time,
) (bool, string) {
	if !rescuePluginPassEvidence(plugin, marker, now) || host.ManualPaused ||
		host.State != OpenAIDowngradeStateOnDuty || host.ProbeMode != "normal" ||
		host.LastProbe == nil {
		return false, ""
	}
	probe := host.LastProbe
	if probe.Mode != "qualification" || probe.At.IsZero() ||
		probe.At.Before(rescueEvidenceStart(marker)) || probe.At.After(now) ||
		now.Sub(probe.At) > openAIRescueEvidenceWindow {
		return false, ""
	}
	result := OpenAIDowngradeProbeResult{
		TransportOK: probe.TransportOK, HTTPStatus: probe.HTTPStatus,
		AnswerCorrect:   probe.AnswerCorrect != nil && *probe.AnswerCorrect,
		ReasoningTokens: probe.ReasoningTokens,
	}
	if !result.IsQualificationPass() {
		return false, ""
	}
	return true, "plugin_and_host_pass"
}

// Called before staging the scheduling unlock. New accounts are deliberately
// unaffected; only accounts with a rescue marker need the second signature.
func (l *OpenAIRescueLane) qualificationReady(ctx context.Context, account *Account, now time.Time) bool {
	if OpenAIRescueManuallyTerminated(account) {
		return false
	}
	marker := GetOpenAIRescueLaneMarker(account)
	if marker == nil {
		return true
	}
	bridge := l.recoveryBridge(ctx)
	return bridge != nil && rescuePluginPassEvidence(bridge.Accounts[account.ID], marker, now)
}

func (l *OpenAIRescueLane) confirmationThreshold() int {
	return min(openAIRescueConfirmationPasses, l.GraduationThreshold())
}

func (l *OpenAIRescueLane) confirmationReady(ctx context.Context, account *Account, now time.Time) bool {
	cfg := l.config()
	if !cfg.Enabled || cfg.GroupID <= 0 || !isOpenAIRescueAccountActive(account) ||
		len(account.GroupIDs) != 1 || account.GroupIDs[0] != cfg.GroupID {
		return false
	}
	bridge := l.recoveryBridge(ctx)
	if bridge == nil {
		return false
	}
	plugin := bridge.Accounts[account.ID]
	return rescuePluginPassEvidence(plugin, GetOpenAIRescueLaneMarker(account), now) &&
		plugin.ConsecutivePasses >= l.confirmationThreshold()
}

// Early confirmation never lowers the graduation threshold or opens scheduling.
func (l *OpenAIRescueLane) maybeConfirm(ctx context.Context, account *Account, plugin *OpenAIPluginBridgeAccount,
	host OpenAIProbeHealthSnapshot, now time.Time,
) error {
	marker := GetOpenAIRescueLaneMarker(account)
	if l.needleTrigger == nil || account.Status != StatusActive || host.ManualPaused ||
		host.State != OpenAIDowngradeStatePendingReplace ||
		!rescuePluginPassEvidence(plugin, marker, now) ||
		plugin.ConsecutivePasses < l.confirmationThreshold() ||
		(!marker.AutoNeedleAt.IsZero() &&
			now.Sub(marker.AutoNeedleAt) < openAIRescueAutoNeedleCooldownFor(marker.AutoNeedleAttempts)) {
		return nil
	}
	if l.schedulingGate != nil {
		allowed, err := l.schedulingGate(ctx, account.ID)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
	}
	return l.autoNeedle(ctx, account)
}

// Poll only bridge candidates. No full roster scan, reseeding or rebind belongs
// in this fast path; all writes retain the existing revision-bound transactions.
func (l *OpenAIRescueLane) runConfirmationSweep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil {
		return nil
	}
	if err := l.loopContext.Err(); err != nil {
		return err
	}
	if !l.sweepMu.TryLock() {
		return nil
	}
	defer l.sweepMu.Unlock()
	cfg := l.config()
	if !cfg.Enabled || cfg.GroupID <= 0 || l.probeStates == nil {
		return nil
	}
	bridge := l.recoveryBridge(ctx)
	if bridge == nil {
		return nil
	}
	now := l.now()
	ids := make([]int64, 0, len(bridge.Accounts))
	for id, plugin := range bridge.Accounts {
		if id > 0 && plugin != nil && !plugin.InBackoff && !plugin.SuspectAccountLevel &&
			plugin.ConsecFails == 0 && plugin.LastVerdict == "pass" &&
			plugin.ConsecutivePasses >= l.confirmationThreshold() &&
			!plugin.LastProbeAt.After(now) && now.Sub(plugin.LastProbeAt) <= openAIRescueEvidenceWindow {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	accounts, err := l.accounts.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}
	eligible := make([]*Account, 0, len(accounts))
	ids = ids[:0]
	for _, account := range accounts {
		if account != nil && account.Status == StatusActive &&
			isOpenAIDowngradeProbeAccountEligible(account, now) && isOpenAIRescueAccountActive(account) &&
			len(account.GroupIDs) == 1 && account.GroupIDs[0] == cfg.GroupID {
			eligible = append(eligible, account)
			ids = append(ids, account.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	states, err := l.probeStates(ctx, ids)
	if err != nil {
		return err
	}
	var result error
	for _, account := range eligible {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		plugin, host := bridge.Accounts[account.ID], states[account.ID]
		recovered, _ := rescueLaneRecoveryEvidence(plugin, host, GetOpenAIRescueLaneMarker(account), now)
		if recovered && plugin.ConsecutivePasses >= l.GraduationThreshold() {
			result = errors.Join(result, l.GraduateRescue(ctx, account.ID, "confirmation_converge"))
			continue
		}
		result = errors.Join(result, l.maybeConfirm(ctx, account, plugin, host, now))
	}
	return result
}

func (l *OpenAIRescueLane) rescueObservationEvent(ctx context.Context, account *Account, event string, details map[string]any) {
	if l.events != nil {
		if err := l.events.AppendOpenAIDowngradeEvent(ctx, account.ID, account.ProxyID, event, details); err != nil {
			slog.Warn("openai_rescue_observation_event_failed", "account_id", account.ID, "event", event, "error", err)
		}
	}
}

func (l *OpenAIRescueLane) observePluginState(ctx context.Context, account *Account,
	marker *OpenAIRescueLaneMarker, plugin *OpenAIPluginBridgeAccount, now time.Time,
) error {
	observation := openAIRescueObservation{}
	if marker.Observation != nil {
		observation = *marker.Observation
		observation.StateLosses = append([]time.Time(nil), observation.StateLosses...)
	}
	changed, lostEvent, backoffEvent, alert := false, false, false, false
	if plugin == nil {
		if !observation.StateLost && (observation.PluginSeen || marker.SeedOK || GetOpenAIRescueSuspected(account)) {
			recent := observation.StateLosses[:0]
			for _, at := range observation.StateLosses {
				if !at.Before(now.Add(-24*time.Hour)) && !at.After(now) {
					recent = append(recent, at)
				}
			}
			// Only two timestamps are needed for the rolling 24h threshold.
			if len(recent) > 1 {
				recent = recent[len(recent)-1:]
			}
			observation.StateLosses = append(recent, now.UTC())
			observation.StateLost = true
			lostEvent, changed = true, true
			alert = len(observation.StateLosses) >= 2
		}
	} else {
		if observation.StateLost || !observation.PluginSeen {
			observation.StateLost, observation.PluginSeen = false, true
			changed = true
		}
		zeroFailureBackoff := plugin.InBackoff && plugin.ConsecFails < 1
		if observation.BackoffObserved != zeroFailureBackoff {
			observation.BackoffObserved = zeroFailureBackoff
			changed, backoffEvent = true, zeroFailureBackoff
		}
	}
	if !changed {
		return nil
	}
	next := *marker
	next.Observation = &observation
	if err := l.commitMarker(ctx, account, &next, false); err != nil {
		return err
	}
	marker.Observation = &observation
	if lostEvent {
		l.rescueObservationEvent(ctx, account, OpenAIDowngradeEventRescuePluginStateLost,
			map[string]any{"suspected": GetOpenAIRescueSuspected(account), "state_losses_24h_at_least": len(observation.StateLosses)})
	}
	if alert {
		l.rescueObservationEvent(ctx, account, OpenAIDowngradeEventRescuePluginStateLostAlert,
			map[string]any{"threshold": 2, "window_hours": 24})
		slog.Warn("openai_rescue_plugin_state_loss_alert", "account_id", account.ID, "window_hours", 24)
	}
	if backoffEvent {
		l.rescueObservationEvent(ctx, account, OpenAIDowngradeEventRescueBackoffObserved,
			map[string]any{"consec_fails": plugin.ConsecFails, "basis": "no_quality_failure"})
	}
	return nil
}
