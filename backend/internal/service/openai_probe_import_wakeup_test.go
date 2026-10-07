package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdminCreateAccountWakesQualificationAfterCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		err  error
		want int
	}{
		{"committed OAuth", AccountTypeOAuth, nil, 1},
		{"failed OAuth", AccountTypeOAuth, errors.New("commit rejected"), 0},
		{"other account type", AccountTypeAPIKey, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &atomicAccountCreateTestRepo{err: tc.err}
			wakes := 0
			svc := &adminServiceImpl{
				accountRepo: repo, accountDuplicateRepo: repo,
				openAIProbeWakeup: func() {
					require.Equal(t, 1, repo.calls)
					require.Equal(t, []AccountGroup{{GroupID: 9, Priority: 1}}, repo.groups)
					wakes++
				},
			}
			input := atomicAccountCreateInput([]int64{9})
			input.Type = tc.kind
			input.Credentials = map[string]any{"refresh_token": "fixture"}
			_, err := svc.CreateAccount(t.Context(), input)
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.want, wakes)
		})
	}
}

func TestOpenAIProbeWakeCoalescesAndRespectsStop(t *testing.T) {
	var absent *OpenAIDowngradeProbeRunner
	absent.Wake()
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	for range 100 {
		runner.Wake()
	}
	require.Len(t, runner.wakeCh, 1)
	<-runner.wakeCh
	runner.Stop()
	runner.Wake()
	require.Empty(t, runner.wakeCh)
}

func TestOpenAIProbeWakeRunsWithoutWaitingForMinuteTicker(t *testing.T) {
	scanned := make(chan struct{}, 1)
	repo := &downgradeProbeAccountRepoStub{
		listByPlatformFn: func(context.Context, string) ([]Account, error) {
			scanned <- struct{}{}
			return nil, nil
		},
	}
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, repo, nil, nil, nil, nil)
	runner.Start()
	t.Cleanup(runner.Stop)
	runner.Wake()
	select {
	case <-scanned:
	case <-time.After(2 * time.Second):
		t.Fatal("committed upload did not wake the probe loop")
	}
}

func TestOpenAIProbeStagedRunnerPreservesPluginTransport(t *testing.T) {
	called := false
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	runner.SetPluginRoundTrip(func(context.Context, *http.Request, string, *Account) (*http.Response, bool, error) {
		called = true
		return nil, true, nil
	})
	stage := newOpenAIProbeStaging(runner, &Account{ID: 71}, &OpenAIDowngradeProbeState{AccountID: 71})
	staged := runner.stagedRunner(stage)
	require.NotNil(t, staged.pluginRoundTrip, "transactional qualification must use the installed rescue transport")
	_, handled, err := staged.pluginRoundTrip(t.Context(), nil, "", stage.account)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, called)
}

func TestOpenAIProbeImportWakeYieldsOldQueue(t *testing.T) {
	for _, wakeAt := range []string{"before_scan", "during_listing", "during_probe"} {
		t.Run(wakeAt, func(t *testing.T) {
			now := time.Now()
			base := &downgradeProbeStoreStub{
				due: []OpenAIDowngradeProbeState{
					{AccountID: 71, State: OpenAIDowngradeStateOnDuty},
					{AccountID: 72, State: OpenAIDowngradeStateOnDuty},
				},
			}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base}
			repo := &downgradeProbeAccountRepoStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			if wakeAt == "before_scan" {
				runner.Wake()
			}
			base.listDueFn = func(context.Context, time.Time, int) ([]OpenAIDowngradeProbeState, error) {
				if wakeAt == "during_listing" {
					runner.Wake()
				}
				return base.due, nil
			}
			var processed []int64
			repo.getByIDFn = func(_ context.Context, id int64) (*Account, error) {
				processed = append(processed, id)
				if wakeAt == "during_probe" {
					runner.Wake()
				}
				return nil, errors.New("fixture lookup failed")
			}
			require.NoError(t, runner.RunOnce(t.Context()))
			switch wakeAt {
			case "before_scan":
				require.Equal(t, []int64{71, 72}, processed, "an existing wake must not starve its own scan")
			case "during_listing":
				require.Empty(t, processed, "refresh a stale queue before starting network work")
			case "during_probe":
				require.Equal(t, []int64{71}, processed, "yield even when the current account failed")
			}
			require.Len(t, runner.wakeCh, 1, "the loop must still receive the rescan request")

			<-runner.wakeCh
			wakeAt = ""
			processed = nil
			require.NoError(t, runner.RunOnce(t.Context()))
			require.Equal(t, []int64{71, 72}, processed, "yielding must not discard the remaining accounts")
		})
	}
}

func TestOpenAIProbeImportWakePreservesInFlightCommit(t *testing.T) {
	now := time.Now()
	proxyID := int64(3)
	account := &Account{
		ID: 71, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, ProxyID: &proxyID, UpdatedAt: now.Add(-time.Hour),
	}
	repo := &downgradeProbeAccountRepoStub{
		account: account,
		listByPlatformFn: func(context.Context, string) ([]Account, error) {
			return nil, nil
		},
	}
	base := &downgradeProbeStoreStub{
		due: []OpenAIDowngradeProbeState{
			{
				AccountID: 71, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, UpdatedAt: now.Add(-time.Minute),
			},
			{AccountID: 72, State: OpenAIDowngradeStateOnDuty},
		},
	}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(ctx context.Context, candidate *Account, _ string) OpenAIDowngradeProbeResult {
		runner.Wake()
		require.NoError(t, ctx.Err(), "an upload must not cancel another account's in-flight probe")
		return OpenAIDowngradeProbeResult{
			AccountID: candidate.ID, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
			TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1500),
		}
	}
	require.NoError(t, runner.RunOnce(t.Context()))
	require.Equal(t, 1, store.commits)
	require.EqualValues(t, 71, store.observed.AccountID)
	require.Len(t, store.observed.Results, 1)
	require.True(t, account.Schedulable)
	require.Equal(t, "normal", base.due[0].ProbeMode)
	require.Equal(t, 1, repo.snapshotCalls)
	require.Len(t, runner.wakeCh, 1)
}

func TestOpenAIProbeAtomicQualificationUsesRescueTransport(t *testing.T) {
	generators := openAIDowngradeProbeDomainGenerators
	openAIDowngradeProbeDomainGenerators = []func(time.Time) openAIDowngradeProbeQuestion{openAIDowngradeCandyQuestion}
	t.Cleanup(func() { openAIDowngradeProbeDomainGenerators = generators })
	for _, pluginErr := range []error{nil, errors.New("fixture plugin unavailable")} {
		t.Run(errorTestName(pluginErr), func(t *testing.T) {
			legacyCalls := 0
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{
				doProbe: func() (*http.Response, error) {
					legacyCalls++
					return nil, errors.New("unexpected legacy transport")
				},
			})
			now := time.Now()
			require.NotNil(t, account.ProxyID)
			account.Status, account.Schedulable = StatusActive, false
			account.UpdatedAt = now.Add(-time.Hour)
			account.Extra = map[string]any{
				OpenAIOAuthQualifiedProxyExtraKey: *account.ProxyID,
				openAIRescueLaneExtraKey: map[string]any{
					"entered_at": now.Add(-time.Minute).UTC().Format(time.RFC3339Nano),
				},
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			base := &downgradeProbeStoreStub{}
			store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: base, accountRepo: repo}
			runner.accountRepo, runner.store = repo, store
			runner.now = func() time.Time { return now }
			runner.nextDelay = func() time.Duration { return time.Minute }
			pluginCalls := 0
			runner.SetPluginRoundTrip(func(_ context.Context, req *http.Request, route string, candidate *Account) (*http.Response, bool, error) {
				pluginCalls++
				require.Equal(t, account.Proxy.URL(), route)
				require.Equal(t, account.ID, candidate.ID)
				require.NotNil(t, GetOpenAIRescueLaneMarker(candidate))
				require.Equal(t, http.MethodPost, req.Method)
				if pluginErr != nil {
					return nil, true, pluginErr
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body: io.NopCloser(strings.NewReader(
						"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}],\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":1992}}}}\n\n")),
				}, true, nil
			})
			state := OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				CurrentProxyID: account.ProxyID, OriginalProxyID: account.ProxyID,
				UpdatedAt: now.Add(-time.Minute), NextProbeAt: now,
			}

			require.NoError(t, runner.processStateAtomic(t.Context(), &state, now))
			require.Equal(t, 1, pluginCalls)
			require.Zero(t, legacyCalls, "handled plugin failures must not silently switch transports")
			require.Equal(t, 1, store.commits)
			require.Len(t, store.observed.Results, 1)
			require.Empty(t, base.proxyChanges)
			require.Zero(t, base.probeCalls, "results must be published by the atomic commit")
			result := store.observed.Results[0]
			require.Equal(t, account.ProxyID, result.ProxyID)
			if pluginErr != nil {
				require.False(t, result.TransportOK)
				require.False(t, account.Schedulable)
			} else {
				require.True(t, result.IsQualificationPass())
				require.False(t, account.Schedulable, "a host probe alone cannot graduate a rescue account")
				require.Equal(t, "qualification", state.ProbeMode)
				require.NotNil(t, GetOpenAIRescueLaneMarker(account))
			}
		})
	}
}
