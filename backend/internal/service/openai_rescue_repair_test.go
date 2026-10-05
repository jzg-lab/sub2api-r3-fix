package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRescueAccountEditPreservesCurrentState(t *testing.T) {
	marker := map[string]any{"orig_group_ids": []any{float64(5)}}
	for _, tc := range []struct {
		name              string
		current, incoming map[string]any
		want              any
	}{
		{"entered_after_form_open", map[string]any{openAIRescueLaneExtraKey: marker}, map[string]any{"custom": "edited"}, marker},
		{"graduated_after_form_open", map[string]any{}, map[string]any{openAIRescueLaneExtraKey: marker}, nil},
		{"empty_extra", map[string]any{openAIRescueLaneExtraKey: marker}, map[string]any{}, marker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &upstreamBillingProbeAdminRepo{upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
				110: {ID: 110, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Extra: tc.current},
			}}}
			updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 110, &UpdateAccountInput{Extra: tc.incoming})
			require.NoError(t, err)
			require.Equal(t, tc.want, updated.Extra[openAIRescueLaneExtraKey])
		})
	}
}

func TestOpenAIRescueCreateStartsWithoutManagedState(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(map[bool]string{false: "account_service", true: "admin_create_and_duplicate"}[admin], func(t *testing.T) {
			extra := map[string]any{openAIRescueLaneExtraKey: "old_marker", openAIRescueRescueCountKey: 3, "openai_rescue_future_key": true, "custom": "kept"}
			var account *Account
			var err error
			if admin {
				account, err = buildAccountForCreate(&CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, extra)
			} else {
				account, err = NewAccountService(&accountServiceCreateRepoStub{}, nil).Create(t.Context(), CreateAccountRequest{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra})
			}
			require.NoError(t, err)
			require.NotContains(t, account.Extra, openAIRescueLaneExtraKey)
			require.NotContains(t, account.Extra, openAIRescueRescueCountKey)
			require.NotContains(t, account.Extra, "openai_rescue_future_key")
			require.Equal(t, "kept", account.Extra["custom"])
			require.True(t, account.Schedulable)
			require.Equal(t, "old_marker", extra[openAIRescueLaneExtraKey])
		})
	}
}

func TestOpenAIRescueAdminExtraWritesIgnoreManagedState(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		t.Run(map[bool]string{false: "extra", true: "bulk"}[bulk], func(t *testing.T) {
			repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: {ID: 1, Extra: map[string]any{}}}}
			svc := &adminServiceImpl{accountRepo: repo}
			updates := map[string]any{openAIRescueLaneExtraKey: "injected", "openai_rescue_future_key": true, "custom": "edited"}
			if bulk {
				_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Extra: updates})
				require.NoError(t, err)
				updates = repo.bulkUpdates[0].Extra
			} else {
				require.NoError(t, svc.UpdateAccountExtra(context.Background(), 1, updates))
				updates = repo.accounts[1].Extra
			}
			require.NotContains(t, updates, openAIRescueLaneExtraKey)
			require.NotContains(t, updates, "openai_rescue_future_key")
			require.Equal(t, "edited", updates["custom"])
		})
	}
}

func TestOpenAIRescueStagedProbeUsesPluginTransport(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		rescue, handled, proxy bool
		pluginErr              error
	}{
		{"rescue_direct", true, true, false, nil},
		{"rescue_proxy", true, true, true, nil},
		{"plugin_declines", true, false, false, nil},
		{"plugin_error", true, true, false, errors.New("plugin unavailable")},
		{"ordinary_direct", false, true, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pluginCalls, upstreamCalls := 0, 0
			response := func() *http.Response {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}
			}
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{doProbe: func() (*http.Response, error) {
				upstreamCalls++
				return response(), nil
			}})
			if !tc.proxy {
				account.ProxyID, account.Proxy = nil, nil
			}
			if tc.rescue {
				account.Extra = map[string]any{}
				rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: time.Now().UTC(), OrigGroupIDs: []int64{5}})
			}
			runner.SetPluginRoundTrip(func(_ context.Context, _ *http.Request, proxyURL string, candidate *Account) (*http.Response, bool, error) {
				pluginCalls++
				require.Equal(t, account.ID, candidate.ID)
				if tc.proxy {
					require.NotEmpty(t, proxyURL)
				} else {
					require.Empty(t, proxyURL)
				}
				if tc.pluginErr != nil {
					return nil, true, tc.pluginErr
				}
				if !tc.handled {
					return nil, false, nil
				}
				return response(), true, nil
			})
			stage := newOpenAIProbeStaging(runner, account, &OpenAIDowngradeProbeState{AccountID: account.ID})
			staged := runner.stagedRunner(stage)
			require.Nil(t, staged.rescueLane, "staging cannot mutate live rescue state")
			staged.probe(context.Background(), stage.account, "qualification")
			if tc.rescue {
				require.Positive(t, pluginCalls)
			} else {
				require.Zero(t, pluginCalls)
			}
			if tc.rescue && tc.handled {
				require.Zero(t, upstreamCalls)
			} else {
				require.Positive(t, upstreamCalls)
			}
		})
	}
}

type rescueQualificationRepo struct {
	*downgradeProbeAccountRepoStub
	lane *rescueLaneRepo
}

type rescueMarkerAtomicRepo struct {
	*rescueLaneRepo
	updated        bool
	expected, next any
}

func (r *rescueMarkerAtomicRepo) UpdateOpenAIRescueMarker(_ context.Context, _ int64, expected, next any) (bool, error) {
	r.expected, r.next = expected, next
	return r.updated, nil
}

func (r *rescueMarkerAtomicRepo) GraduateOpenAIRescue(context.Context, int64, any, []int64, map[string]any) (bool, error) {
	return false, errors.New("unexpected graduation")
}

func (r *rescueMarkerAtomicRepo) CommitOpenAIRescueMarker(_ context.Context, m OpenAIRescueMarkerUpdate) (time.Time, error) {
	r.expected, r.next = r.account.Extra[openAIRescueLaneExtraKey], m.RawMarker
	if !r.updated {
		return time.Time{}, ErrOpenAIProbeStale
	}
	return r.account.UpdatedAt.Add(time.Microsecond), nil
}

func TestOpenAIRescueMarkerUpdatesUseCurrentMarkerGuard(t *testing.T) {
	for _, updated := range []bool{false, true} {
		t.Run(fmt.Sprint(updated), func(t *testing.T) {
			account := rescueLaneSweepAccount(71)
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: time.Now().UTC(), OrigGroupIDs: []int64{5}})
			repo := &rescueMarkerAtomicRepo{rescueLaneRepo: &rescueLaneRepo{account: account}, updated: updated}
			err := NewOpenAIRescueLane(repo, nil, nil, nil).updateMarkerFields(t.Context(), account.ID, func(marker *OpenAIRescueLaneMarker) { marker.SeedOK = true })
			if updated {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrOpenAIProbeStale)
			}
			require.Equal(t, account.Extra[openAIRescueLaneExtraKey], repo.expected)
			require.Equal(t, true, repo.next.(map[string]any)["seed_ok"])
			require.Empty(t, repo.extraSets, "a rejected stale update cannot fall back to an unconditional write")
		})
	}
}

func TestOpenAIRescueAccountingPreservesRawSnapshot(t *testing.T) {
	for _, action := range []string{"seed_success", "seed_failure", "auto_needle", "observation"} {
		for _, snapshot := range []any{"missing", nil, "broken", []any{5, "bad"}, []any{1.5}, []int64{0}, []int64{5, 5}} {
			t.Run(fmt.Sprintf("%s/%v", action, snapshot), func(t *testing.T) {
				account := rescueLaneSweepAccount(71)
				fields := map[string]any{"entered_at": time.Now().UTC().Format(time.RFC3339Nano), "orig_group_ids": snapshot, "future_field": "kept", "seed_ok": false}
				if snapshot == "missing" {
					delete(fields, "orig_group_ids")
				}
				account.Extra[openAIRescueLaneExtraKey] = fields
				repo := &rescueLaneRepo{account: account}
				seedErr := errors.New("temporary transport failure")
				lane := NewOpenAIRescueLane(repo, nil, nil, func(context.Context, int64) error {
					if action == "seed_failure" {
						return seedErr
					}
					return nil
				})
				if action == "observation" {
					require.NoError(t, lane.observePluginState(t.Context(), account, GetOpenAIRescueLaneMarker(account), &OpenAIPluginBridgeAccount{}, time.Now().UTC()))
				} else if action == "auto_needle" {
					lane.SetNeedleTrigger(func(context.Context, int64) error { return nil })
					lane.autoNeedle(t.Context(), account)
				} else {
					err := lane.seedAndAccount(t.Context(), account, "test")
					if action == "seed_failure" {
						require.ErrorIs(t, err, seedErr)
					} else {
						require.NoError(t, err)
					}
				}
				after := account.Extra[openAIRescueLaneExtraKey].(map[string]any)
				require.Equal(t, fields["orig_group_ids"], after["orig_group_ids"])
				_, beforePresent := fields["orig_group_ids"]
				_, afterPresent := after["orig_group_ids"]
				require.Equal(t, beforePresent, afterPresent)
				require.Equal(t, fields["entered_at"], after["entered_at"])
				require.Equal(t, "kept", after["future_field"])
				require.Error(t, lane.GraduateRescue(t.Context(), account.ID, "test"))
			})
		}
	}
}

func TestOpenAIRescueSweepExitKeepsNewEpisode(t *testing.T) {
	for _, reentered := range []bool{false, true} {
		t.Run(fmt.Sprint(reentered), func(t *testing.T) {
			account := rescueLaneSweepAccount(71)
			account.GroupIDs = []int64{99}
			oldEpisode := time.Now().UTC()
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: oldEpisode, OrigGroupIDs: []int64{5}, ExitReason: openAIRescueExitAuthRejected})
			repo := &rescueLaneRepo{roster: []Account{*account}}
			sink := &rescueLaneSink{}
			lane := NewOpenAIRescueLane(repo, sink, rescueLaneEnabledConfig, nil)
			lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
				repo.roster[0].Extra = map[string]any{}
				if reentered {
					rescueLaneApplyMarker(t, &repo.roster[0], OpenAIRescueLaneMarker{EnteredAt: oldEpisode.Add(time.Second), OrigGroupIDs: []int64{6}})
				}
				return nil, nil
			})
			_, _, _, err := lane.RunReconcileSweep(t.Context())
			require.NoError(t, err)
			require.Empty(t, repo.binds)
			require.Empty(t, repo.extraSets)
			require.Empty(t, sink.events)
			if reentered {
				require.Equal(t, oldEpisode.Add(time.Second), GetOpenAIRescueLaneMarker(&repo.roster[0]).EnteredAt)
			}
		})
	}
}

func (r *rescueQualificationRepo) BindGroups(ctx context.Context, id int64, groups []int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.lane.BindGroups(ctx, id, groups)
}

func (r *rescueQualificationRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	return r.lane.UpdateExtra(ctx, id, updates)
}

func (r *rescueQualificationRepo) CommitOpenAIRescueGraduation(ctx context.Context, m OpenAIRescueGraduation) error {
	return r.lane.CommitOpenAIRescueGraduation(ctx, m)
}

func TestOpenAIRescueQualificationGraduatesOnlyAfterCommit(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		commitErr, snapshotErr error
		cancelAfterCommit      bool
	}{
		{"success", nil, nil, false},
		{"stale", ErrOpenAIProbeStale, nil, false},
		{"commit_failure", errors.New("commit failed"), nil, false},
		{"cache_failure", nil, errors.New("cache unavailable"), false},
		{"canceled_after_commit", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, GroupIDs: []int64{99}, UpdatedAt: now, Extra: map[string]any{}}
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{5}})
			baseRepo := &downgradeProbeAccountRepoStub{account: account, snapshotErr: tc.snapshotErr}
			laneRepo := &rescueLaneRepo{account: account}
			repo := &rescueQualificationRepo{baseRepo, laneRepo}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: baseRepo, commitErr: tc.commitErr}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store.afterCommit = func() {
				require.Empty(t, laneRepo.binds)
				if tc.cancelAfterCommit {
					cancel()
				}
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.SetRescueLane(NewOpenAIRescueLane(repo, &rescueLaneSink{}, rescueLaneEnabledConfig, nil))
			rescueLaneSetRecoveryEvidence(runner.rescueLane, account.ID, now)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				require.Empty(t, laneRepo.binds)
				return OpenAIDowngradeProbeResult{AccountID: 7, HTTPStatus: http.StatusOK, TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500)}
			}
			state := OpenAIDowngradeProbeState{AccountID: 7, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification", UpdatedAt: now}
			err := runner.processStateAtomic(ctx, &state, now)
			if tc.commitErr != nil {
				require.ErrorIs(t, err, tc.commitErr)
				require.Empty(t, laneRepo.binds)
				require.NotNil(t, GetOpenAIRescueLaneMarker(account))
			} else {
				if tc.snapshotErr != nil {
					require.ErrorIs(t, err, ErrOpenAIProbeSnapshotRefresh)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, []int64{5}, account.GroupIDs)
				require.Nil(t, GetOpenAIRescueLaneMarker(account))
				require.Equal(t, "normal", state.ProbeMode)
				require.True(t, account.Schedulable)
			}
		})
	}
}

func TestOpenAIRescueGraduationRejectsInvalidOriginalGroups(t *testing.T) {
	for _, snapshot := range []any{"missing", "broken", []any{5, "bad"}, []any{1.5}, []int64{0}} {
		t.Run(fmt.Sprint(snapshot), func(t *testing.T) {
			account := rescueLaneSweepAccount(71)
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: time.Now().UTC(), OrigGroupIDs: []int64{5}})
			fields := account.Extra[openAIRescueLaneExtraKey].(map[string]any)
			if snapshot == "missing" {
				delete(fields, "orig_group_ids")
			} else {
				fields["orig_group_ids"] = snapshot
			}
			repo := &rescueLaneRepo{account: account}
			err := NewOpenAIRescueLane(repo, nil, nil, nil).GraduateRescue(t.Context(), account.ID, "test")
			require.ErrorContains(t, err, "rescue original group snapshot")
			require.Empty(t, repo.binds)
			require.Empty(t, repo.extraSets)
		})
	}
}

func TestOpenAIRescueDisabledSweepFinishesGraduationOnly(t *testing.T) {
	for _, missingGroup := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "missing_group"}[missingGroup], func(t *testing.T) {
			cfg := rescueLaneEnabledConfig()
			if missingGroup {
				cfg.GroupID = 0
			} else {
				cfg.Enabled = false
			}
			now := time.Now().UTC()
			accounts := []Account{*rescueLaneSweepAccount(71), *rescueLaneSweepAccount(72), *rescueLaneSweepAccount(73)}
			for i := 0; i < 2; i++ {
				rescueLaneApplyMarker(t, &accounts[i], OpenAIRescueLaneMarker{EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{5}})
				accounts[i].GroupIDs = []int64{99}
			}
			repo := &rescueLaneRepo{roster: accounts}
			lane := NewOpenAIRescueLane(repo, &rescueLaneSink{}, func() OpenAIRescueLaneConfig { return cfg }, nil)
			lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
				return &PluginBridgeStatus{Running: true, Healthy: true,
					StatusJSON: rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(71, false, 6, 0, false, "", now))}
			})
			lane.SetNeedleTrigger(func(context.Context, int64) error { t.Fatal("disabled sweep must not probe"); return nil })
			lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
				return map[int64]OpenAIProbeHealthSnapshot{
					71: rescueRecoverySnapshot(71, now),
					72: {State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification"},
					73: {State: OpenAIDowngradeStatePendingReplace, ProbeMode: "normal"},
				}, nil
			})
			entered, healed, withdrawn, err := lane.RunReconcileSweep(context.Background())
			require.NoError(t, err)
			require.Equal(t, 0, entered)
			require.Equal(t, 1, healed)
			require.Equal(t, 0, withdrawn)
			require.Equal(t, [][]int64{{5}}, repo.binds)
			require.NotNil(t, GetOpenAIRescueLaneMarker(&repo.roster[1]))
			require.Equal(t, []bool{true}, repo.schedSets)
		})
	}
}
