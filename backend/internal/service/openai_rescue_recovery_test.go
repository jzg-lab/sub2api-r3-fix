package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRescueMarkerPersistenceMatchesLocalSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 4, 1, 2, 3, 456789123, time.UTC)
	for _, marker := range []OpenAIRescueLaneMarker{
		{EnteredAt: now, OrigGroupIDs: []int64{3}},
		{
			EnteredAt: now, OrigGroupIDs: []int64{3}, SeedOK: true,
			LastSeedAt: now, SeedAttempts: 1, AutoNeedleAt: now, AutoNeedleAttempts: 1,
			Observation: &openAIRescueObservation{PluginSeen: true, StateLosses: []time.Time{now}},
		},
		{
			EnteredAt: now, OrigGroupIDs: []int64{3},
			Observation: &openAIRescueObservation{
				SuspectedAt: now, PluginSeen: true, StateLost: true,
				StateLosses: []time.Time{now.Add(-time.Hour), now}, BackoffObserved: true,
			},
		},
	} {
		persisted, err := json.Marshal(marker)
		require.NoError(t, err)
		local := rescueLaneTestAccount()
		local.Extra = map[string]any{openAIRescueLaneExtraKey: rescueLaneMarkerExtraValue(marker)}
		stored := *local
		stored.Extra = map[string]any{}
		require.NoError(t, json.Unmarshal(persisted, &stored.Extra))
		stored.Extra = map[string]any{openAIRescueLaneExtraKey: stored.Extra}
		require.Equal(t, openAIProbeInputHash(local), openAIProbeInputHash(&stored),
			"database round trip must not look like a changed seed input")
		require.Equal(t, marker.Observation, GetOpenAIRescueLaneMarker(&stored).Observation)
	}
}

func TestRescueEntryCannotPublishStaleSeedCompletion(t *testing.T) {
	account := rescueLaneTestAccount()
	repo, sink := &rescueLaneRepo{account: account}, &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error {
		account.UpdatedAt = account.UpdatedAt.Add(time.Second)
		delete(account.Extra, openAIRescueLaneExtraKey)
		account.GroupIDs = []int64{3}
		account.Schedulable = true
		return nil
	}
	require.ErrorIs(t, lane.EnterRescue(t.Context(), account.ID, "test"), ErrOpenAIProbeStale)
	require.Nil(t, GetOpenAIRescueLaneMarker(account))
	require.True(t, account.Schedulable)
	require.Empty(t, sink.events)
}

func rescueRecoverySnapshot(id int64, now time.Time) OpenAIProbeHealthSnapshot {
	correct, tokens := true, 1400
	return OpenAIProbeHealthSnapshot{
		AccountID: id, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		LastProbe: &OpenAIProbeLastEvidence{
			ID: 1, At: now, Mode: "qualification", TransportOK: true, HTTPStatus: 200,
			AnswerCorrect: &correct, ReasoningTokens: &tokens,
		},
	}
}

func TestRescueGraduationRequiresConfiguredPassThreshold(t *testing.T) {
	now := time.Now().UTC()
	for _, passes := range []int{2, 3, 5, 6} {
		t.Run(string(rune('0'+passes)), func(t *testing.T) {
			account := rescueLaneSweepAccount(71)
			account.GroupIDs = []int64{99}
			account.Schedulable = false
			rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
				EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{3},
			})
			repo := &rescueLaneRepo{account: account}
			lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
			lane.now = func() time.Time { return now }
			rescueLaneSetRecoveryEvidence(lane, account.ID, now)
			lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
				return &PluginBridgeStatus{Running: true, Healthy: true,
					StatusJSON: rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(71, false, passes, 0, false, "", now))}
			})
			err := lane.GraduateRescue(t.Context(), account.ID, "qualification_pass")
			if passes < lane.GraduationThreshold() {
				require.ErrorIs(t, err, ErrRescueRecoveryUnverified)
				require.False(t, account.Schedulable)
				require.NotNil(t, GetOpenAIRescueLaneMarker(account))
				require.Empty(t, repo.binds)
			} else {
				require.NoError(t, err)
				require.True(t, account.Schedulable)
				require.Nil(t, GetOpenAIRescueLaneMarker(account))
			}
		})
	}
}

func TestRescueGraduationCommitFailureCannotPublishRecovery(t *testing.T) {
	now := time.Now().UTC()
	account := rescueLaneSweepAccount(71)
	account.GroupIDs = []int64{99}
	account.Extra[openAIRescueSuspectedExtraKey] = true
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: now.Add(-time.Hour), OrigGroupIDs: []int64{3},
	})
	commitErr := errors.New("graduation transaction failed")
	repo := &rescueLaneRepo{account: account, graduateErr: commitErr}
	sink := &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.now = func() time.Time { return now }
	rescueLaneSetRecoveryEvidence(lane, account.ID, now)
	require.ErrorIs(t, lane.GraduateRescue(t.Context(), account.ID, "qualification_pass"), commitErr)
	require.Empty(t, sink.events)
	require.Empty(t, repo.binds)
	require.Empty(t, repo.extraSets)
	require.NotNil(t, GetOpenAIRescueLaneMarker(account))
	require.True(t, GetOpenAIRescueSuspected(account))
	require.Equal(t, []int64{99}, account.GroupIDs)
}

func TestRescueMarkerCommitFailureCannotMutateOrPublish(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		t.Run(map[bool]string{false: "observation", true: "withdraw"}[withdraw], func(t *testing.T) {
			account := rescueLaneMarkedAccount(t)
			account.Schedulable = true
			marker := GetOpenAIRescueLaneMarker(account)
			marker.SeedOK = true
			rescueLaneApplyMarker(t, account, *marker)
			commitErr := errors.New("marker transaction failed")
			repo := &rescueLaneRepo{account: account, markerErr: commitErr}
			sink := &rescueLaneSink{}
			lane := newRescueLaneTestLane(repo, sink)
			snapshot, err := repo.GetByID(t.Context(), account.ID)
			require.NoError(t, err)
			if withdraw {
				require.False(t, lane.withdrawScheduling(t.Context(), snapshot,
					&OpenAIPluginBridgeAccount{InBackoff: true, ConsecFails: 1}))
			} else {
				require.ErrorIs(t, lane.observePluginState(t.Context(), snapshot,
					GetOpenAIRescueLaneMarker(snapshot), nil, time.Now()), commitErr)
			}
			require.True(t, account.Schedulable)
			require.False(t, GetOpenAIRescueSuspected(account))
			require.Nil(t, GetOpenAIRescueLaneMarker(account).Observation)
			require.Nil(t, GetOpenAIRescueLaneMarker(snapshot).Observation)
			require.Empty(t, sink.events)
			require.Empty(t, repo.extraSets)
			require.Empty(t, repo.schedSets)
		})
	}
}

func TestRescueStaleObservationCannotUndoGraduationOrReauthorization(t *testing.T) {
	for _, withdraw := range []bool{false, true} {
		for _, removeMarker := range []bool{false, true} {
			t.Run(map[bool]string{false: "observation", true: "withdraw"}[withdraw]+
				map[bool]string{false: "_reauthorized", true: "_graduated"}[removeMarker], func(t *testing.T) {
				account := rescueLaneMarkedAccount(t)
				marker := GetOpenAIRescueLaneMarker(account)
				marker.SeedOK = true
				rescueLaneApplyMarker(t, account, *marker)
				repo, sink := &rescueLaneRepo{account: account}, &rescueLaneSink{}
				lane := newRescueLaneTestLane(repo, sink)
				stale, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				account.UpdatedAt = account.UpdatedAt.Add(time.Second)
				account.Schedulable = true
				if removeMarker {
					delete(account.Extra, openAIRescueLaneExtraKey)
				}
				if withdraw {
					require.False(t, lane.withdrawScheduling(t.Context(), stale,
						&OpenAIPluginBridgeAccount{InBackoff: true, ConsecFails: 2}))
				} else {
					require.ErrorIs(t, lane.observePluginState(t.Context(), stale,
						GetOpenAIRescueLaneMarker(stale), nil, time.Now()), ErrOpenAIProbeStale)
				}
				require.True(t, account.Schedulable)
				require.False(t, GetOpenAIRescueSuspected(account))
				require.Empty(t, sink.events)
				require.Empty(t, repo.extraSets)
				require.Empty(t, repo.schedSets)
				if removeMarker {
					require.Nil(t, GetOpenAIRescueLaneMarker(account))
				} else {
					require.Nil(t, GetOpenAIRescueLaneMarker(account).Observation)
				}
			})
		}
	}
}

func TestRescueInFlightSeedCannotOverwriteNewGeneration(t *testing.T) {
	for _, change := range []string{"graduated", "new_rescue", "credentials", "proxy"} {
		for _, seedErr := range []error{nil, errors.New("API returned 401")} {
			t.Run(change+map[bool]string{false: "_pass", true: "_auth_error"}[seedErr != nil], func(t *testing.T) {
				account := rescueLaneMarkedAccount(t)
				repo, sink := &rescueLaneRepo{account: account}, &rescueLaneSink{}
				lane := newRescueLaneTestLane(repo, sink)
				lane.seed = func(context.Context, int64) error {
					account.UpdatedAt = account.UpdatedAt.Add(time.Second)
					switch change {
					case "graduated":
						delete(account.Extra, openAIRescueLaneExtraKey)
					case "new_rescue":
						marker := GetOpenAIRescueLaneMarker(account)
						marker.EnteredAt = marker.EnteredAt.Add(time.Minute)
						marker.SeedAttempts = 0
						rescueLaneApplyMarker(t, account, *marker)
					case "credentials":
						account.Credentials = map[string]any{"test_revision": 2}
					case "proxy":
						id := int64(123)
						account.ProxyID = &id
					}
					account.Schedulable = true
					return seedErr
				}
				snapshot, err := repo.GetByID(t.Context(), account.ID)
				require.NoError(t, err)
				require.ErrorIs(t, lane.seedAndAccount(t.Context(), snapshot, "test"), ErrOpenAIProbeStale)
				require.True(t, account.Schedulable)
				require.Len(t, repo.extraSets, 1, "only pre-request reservation may commit")
				require.Empty(t, repo.binds)
				require.Empty(t, sink.events)
				if change == "graduated" {
					require.Nil(t, GetOpenAIRescueLaneMarker(account))
				}
			})
		}
	}
}

func TestRescueFailedReservationDoesNotSendUpstreamRequests(t *testing.T) {
	account := rescueLaneMarkedAccount(t)
	repo := &rescueLaneRepo{account: account, markerErr: ErrOpenAIProbeStale}
	lane := newRescueLaneTestLane(repo, &rescueLaneSink{})
	lane.seed = func(context.Context, int64) error {
		t.Fatal("failed seed reservation must not send a request")
		return nil
	}
	lane.needleTrigger = func(context.Context, int64) error {
		t.Fatal("failed needle reservation must not send a request")
		return nil
	}
	require.ErrorIs(t, lane.seedAndAccount(t.Context(), account, "test"), ErrOpenAIProbeStale)
	lane.autoNeedle(t.Context(), account)
}

func TestRescueStaleSweepCannotRebindGraduatedAccount(t *testing.T) {
	account := rescueLaneSweepAccount(66)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: time.Now().Add(-time.Hour)})
	account.GroupIDs = []int64{3, 99}
	repo, sink := &rescueLaneRepo{roster: []Account{*account}}, &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		current := repo.resolve(account.ID)
		current.UpdatedAt = current.UpdatedAt.Add(time.Second)
		current.GroupIDs = []int64{3}
		delete(current.Extra, openAIRescueLaneExtraKey)
		return nil, nil
	})
	_, healed, _, err := lane.RunReconcileSweep(t.Context())
	require.ErrorIs(t, err, ErrOpenAIProbeStale)
	require.Zero(t, healed)
	require.Equal(t, []int64{3}, repo.resolve(account.ID).GroupIDs)
	require.Empty(t, repo.binds)
	require.Empty(t, sink.events)
}

func TestRescueTransitionFailureCannotSendSeedOrPublishExit(t *testing.T) {
	commitErr := errors.New("transition transaction failed")
	account := rescueLaneTestAccount()
	repo, sink := &rescueLaneRepo{account: account, transitionErr: commitErr}, &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.seed = func(context.Context, int64) error {
		t.Fatal("failed entry must not send a seed")
		return nil
	}
	require.ErrorIs(t, lane.EnterRescue(t.Context(), account.ID, "test"), commitErr)
	require.Nil(t, GetOpenAIRescueLaneMarker(account))
	require.Empty(t, sink.events)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{EnteredAt: time.Now().Add(-time.Hour)})
	require.ErrorIs(t, lane.ExitRescue(t.Context(), account.ID, "auth_rejected"), commitErr)
	require.NotNil(t, GetOpenAIRescueLaneMarker(account))
	require.Empty(t, sink.events)
	require.Empty(t, repo.extraSets)
	require.Empty(t, repo.binds)
}

func rescueLaneSetRecoveryEvidence(lane *OpenAIRescueLane, id int64, now time.Time) {
	lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
		return &PluginBridgeStatus{Running: true, Healthy: true,
			StatusJSON: rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(id, false, 6, 0, false, "", now))}
	})
	lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
		return map[int64]OpenAIProbeHealthSnapshot{id: rescueRecoverySnapshot(id, now)}, nil
	})
}

func TestRescueRecoveryRejectsMissingStaleOrContradictoryEvidence(t *testing.T) {
	now := time.Now().UTC()
	cases := map[string]func(*OpenAIPluginBridgeAccount, *OpenAIProbeHealthSnapshot, *OpenAIRescueLaneMarker){
		"single_plugin_pass": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.ConsecutivePasses = 1
		},
		"plugin_last_error": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.LastVerdict = "error"
		},
		"plugin_failures": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.ConsecFails = 1
		},
		"plugin_old": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.LastProbeAt = now.Add(-16 * time.Minute)
		},
		"plugin_future": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.LastProbeAt = now.Add(time.Second)
		},
		"plugin_previous_pass_before_rescue": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.PreviousPassAt = now.Add(-2 * time.Hour)
		},
		"plugin_previous_pass_missing": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.PreviousPassAt = time.Time{}
		},
		"plugin_previous_pass_future": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.PreviousPassAt = now.Add(time.Second)
		},
		"plugin_previous_pass_stale": func(p *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			p.PreviousPassAt = now.Add(-16 * time.Minute)
		},
		"host_wrong": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			*s.LastProbe.AnswerCorrect = false
		},
		"host_no_answer": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.AnswerCorrect = nil
		},
		"host_transport": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.TransportOK = false
		},
		"host_non_200": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.HTTPStatus = 429
		},
		"host_low_budget": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			*s.LastProbe.ReasoningTokens = 799
		},
		"host_missing_budget": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.ReasoningTokens = nil
		},
		"host_old": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.At = now.Add(-16 * time.Minute)
		},
		"host_future": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.At = now.Add(time.Second)
		},
		"host_not_qualification": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe.Mode = "normal"
		},
		"host_missing": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.LastProbe = nil
		},
		"host_failed_again": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.State = OpenAIDowngradeStatePendingReplace
		},
		"manual_pause": func(_ *OpenAIPluginBridgeAccount, s *OpenAIProbeHealthSnapshot, _ *OpenAIRescueLaneMarker) {
			s.ManualPaused = true
		},
		"after_suspicion": func(_ *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, m *OpenAIRescueLaneMarker) {
			m.Observation = &openAIRescueObservation{SuspectedAt: now.Add(time.Second)}
		},
		"after_plugin_restart": func(_ *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, m *OpenAIRescueLaneMarker) {
			m.Observation = &openAIRescueObservation{StateLosses: []time.Time{now.Add(time.Second)}}
		},
		"exiting": func(_ *OpenAIPluginBridgeAccount, _ *OpenAIProbeHealthSnapshot, m *OpenAIRescueLaneMarker) {
			m.ExitReason = "auth_rejected"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			plugin := &OpenAIPluginBridgeAccount{
				ConsecutivePasses: 2, LastVerdict: "pass", LastProbeAt: now, PreviousPassAt: now,
			}
			host := rescueRecoverySnapshot(63, now)
			marker := &OpenAIRescueLaneMarker{EnteredAt: now.Add(-time.Hour)}
			change(plugin, &host, marker)
			ok, basis := rescueLaneRecoveryEvidence(plugin, host, marker, now)
			require.False(t, ok)
			require.Empty(t, basis)
		})
	}
}

func TestRescueStateLossReseedsWithoutRecoveryAndAlertsOnRepeatedEpisodes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	entered, suspected := now.Add(-time.Hour), now.Add(-30*time.Minute)
	account := rescueLaneSweepAccount(65)
	account.GroupIDs = []int64{99}
	account.Extra[openAIRescueSuspectedExtraKey] = true
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: entered, OrigGroupIDs: []int64{3}, SeedOK: true,
		LastSeedAt:  now.Add(-20 * time.Minute),
		Observation: &openAIRescueObservation{SuspectedAt: suspected},
	})
	repo, sink := &rescueLaneRepo{roster: []Account{*account}}, &rescueLaneSink{}
	lane := newRescueLaneTestLane(repo, sink)
	lane.now = func() time.Time { return now }
	seeds := 0
	lane.seed = func(context.Context, int64) error { seeds++; return nil }
	status := rescueLaneBridgeJSON()
	lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
		return &PluginBridgeStatus{Running: true, Healthy: true, StatusJSON: status}
	})
	sweep := func() {
		t.Helper()
		_, healed, withdrawn, err := lane.RunReconcileSweep(context.Background())
		require.NoError(t, err)
		require.Zero(t, healed)
		require.Zero(t, withdrawn)
		require.True(t, GetOpenAIRescueSuspected(repo.resolve(65)))
		marker := GetOpenAIRescueLaneMarker(repo.resolve(65))
		require.Equal(t, entered, marker.EnteredAt)
		require.Equal(t, suspected, marker.Observation.SuspectedAt)
	}
	sweep()
	sweep()
	require.Equal(t, 1, seeds)
	require.Len(t, sink.events, 1)
	require.Equal(t, OpenAIDowngradeEventRescuePluginStateLost, sink.events[0].eventType)
	status = rescueLaneBridgeJSON(rescueLaneBridgeAccountAtJSON(65, false, 1, 0, false, "", now))
	sweep()
	now = now.Add(20 * time.Minute)
	status = rescueLaneBridgeJSON()
	sweep()
	sweep()
	require.Equal(t, 2, seeds)
	require.Len(t, sink.events, 3)
	require.Equal(t, OpenAIDowngradeEventRescuePluginStateLostAlert, sink.events[2].eventType)
	require.Empty(t, repo.schedSets)
	for _, event := range sink.events {
		require.NotEqual(t, OpenAIDowngradeEventRescueRecovered, event.eventType)
	}
}

func TestRescueZeroFailureBackoffIsObservationOnly(t *testing.T) {
	account := rescueLaneSweepAccount(61)
	rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
		EnteredAt: time.Now().Add(-time.Hour), OrigGroupIDs: []int64{3},
	})
	account.GroupIDs = []int64{99}
	repo, sink := &rescueLaneRepo{roster: []Account{*account}}, &rescueLaneSink{}
	lane := rescueLaneSweepLaneWithBridge(repo, sink,
		rescueLaneBridgeJSON(rescueLaneBridgeAccountJSON(61, true, 0, 0, true, "")), true)
	for range 2 {
		_, healed, withdrawn, err := lane.RunReconcileSweep(context.Background())
		require.NoError(t, err)
		require.Zero(t, healed)
		require.Zero(t, withdrawn)
	}
	require.Empty(t, repo.schedSets)
	require.False(t, GetOpenAIRescueSuspected(repo.resolve(61)))
	require.Len(t, sink.events, 1)
	require.Equal(t, OpenAIDowngradeEventRescueBackoffObserved, sink.events[0].eventType)
}

func TestRescueInvalidBridgeCannotRestoreOrReseed(t *testing.T) {
	for _, status := range []*PluginBridgeStatus{
		nil,
		{Running: false, Healthy: true, StatusJSON: rescueLaneBridgeJSON()},
		{Running: true, Healthy: false, StatusJSON: rescueLaneBridgeJSON()},
		{Running: true, Healthy: true, Offline: true, StatusJSON: rescueLaneBridgeJSON()},
		{Running: true, Healthy: true, StatusJSON: "invalid"},
		{Running: true, Healthy: true, StatusJSON: `{"prober":{"enabled":false,"accounts":[]}}`},
	} {
		for _, suspected := range []bool{false, true} {
			for _, seeded := range []bool{false, true} {
				account := rescueLaneSweepAccount(65)
				account.GroupIDs = []int64{99}
				account.Extra[openAIRescueSuspectedExtraKey] = suspected
				rescueLaneApplyMarker(t, account, OpenAIRescueLaneMarker{
					EnteredAt: time.Now().Add(-time.Hour), SeedOK: seeded,
				})
				repo, sink := &rescueLaneRepo{roster: []Account{*account}}, &rescueLaneSink{}
				lane := newRescueLaneTestLane(repo, sink)
				lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus { return status })
				seeds := 0
				lane.seed = func(context.Context, int64) error { seeds++; return nil }
				for range 2 {
					_, healed, _, err := lane.RunReconcileSweep(context.Background())
					require.NoError(t, err)
					require.Zero(t, healed)
				}
				require.Zero(t, seeds, "invalid bridge must not trigger rehydration")
				require.Equal(t, suspected, GetOpenAIRescueSuspected(repo.resolve(65)))
				require.Empty(t, sink.events)

				lane.SetBridgeSource(func(context.Context) *PluginBridgeStatus {
					return &PluginBridgeStatus{Running: true, Healthy: true, StatusJSON: rescueLaneBridgeJSON()}
				})
				_, _, _, err := lane.RunReconcileSweep(context.Background())
				require.NoError(t, err)
				require.Equal(t, 1, seeds, "healthy bridge should resume missing-template recovery")
				require.Equal(t, suspected, GetOpenAIRescueSuspected(repo.resolve(65)))
				for _, event := range sink.events {
					require.NotEqual(t, OpenAIDowngradeEventRescueRecovered, event.eventType)
				}
			}
		}
	}
}

func TestRescueEntryGateFailureCannotMutateOrSeedCandidate(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "denied_with_error", true: "allowed_with_error"}[allowed], func(t *testing.T) {
			repo := &rescueLaneRepo{roster: []Account{
				*rescueLaneSweepAccount(82), *rescueLaneSweepAccount(83),
			}}
			sink := &rescueLaneSink{}
			lane := newRescueLaneTestLane(repo, sink)
			lane.SetProbeStateSource(func(context.Context, []int64) (map[int64]OpenAIProbeHealthSnapshot, error) {
				return map[int64]OpenAIProbeHealthSnapshot{
					82: {AccountID: 82, State: OpenAIDowngradeStatePendingReplace},
					83: {AccountID: 83, State: OpenAIDowngradeStatePendingReplace},
				}, nil
			})
			gateErr := errors.New("pause state unavailable")
			lane.SetSchedulingGate(func(_ context.Context, id int64) (bool, error) {
				if id == 82 {
					return allowed, gateErr
				}
				return true, nil
			})
			var seeded []int64
			lane.seed = func(_ context.Context, id int64) error {
				seeded = append(seeded, id)
				return nil
			}
			before, err := json.Marshal(repo.resolve(82))
			require.NoError(t, err)

			entered, healed, withdrawn, err := lane.RunReconcileSweep(t.Context())
			require.ErrorIs(t, err, gateErr)
			require.Equal(t, 1, entered, "one failed gate must not block another eligible account")
			require.Zero(t, healed)
			require.Zero(t, withdrawn)
			require.Equal(t, []int64{83}, seeded)
			after, err := json.Marshal(repo.resolve(82))
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after))
			require.NotNil(t, GetOpenAIRescueLaneMarker(repo.resolve(83)))
			for _, event := range sink.events {
				require.Equal(t, int64(83), event.accountID)
			}
		})
	}
}
