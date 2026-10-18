package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeImportCooldownPostgres(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		status      int
		probed      bool
		transportOK bool
		mode        string
		unknownIP   bool
		wantDue     bool
	}{
		{name: "deleted import does not hold a new account ten minutes", age: 2 * time.Minute, status: 200, wantDue: true},
		{name: "same exit still has a one minute floor", age: 59 * time.Second, status: 200},
		{name: "one minute boundary", age: time.Minute, status: 200, wantDue: true},
		{name: "unknown exit uses the same proxy floor", age: 59 * time.Second, status: 200, unknownIP: true},
		{name: "unknown exit first admission", age: 2 * time.Minute, status: 200, unknownIP: true, wantDue: true},
		{name: "429 retains the ten minute floor", age: 2 * time.Minute, status: 429},
		{name: "unknown exit 429 retains cooldown", age: 2 * time.Minute, status: 429, unknownIP: true},
		{name: "429 ten minute boundary", age: 10 * time.Minute, status: 429, wantDue: true},
		{name: "repeat qualification retains cooldown", age: 2 * time.Minute, status: 200, probed: true},
		{name: "routine first probe is not an import", age: 2 * time.Minute, status: 200, mode: "normal"},
		{name: "first admission after transport interruption", age: 2 * time.Minute, wantDue: true},
		{name: "interrupted admission retries on schedule", age: 2 * time.Minute, probed: true, wantDue: true},
		{name: "interrupted admission retains one minute floor", age: 59 * time.Second, probed: true},
		{name: "interrupted admission floor boundary", age: time.Minute, probed: true, wantDue: true},
		{name: "unknown exit interrupted admission retries", age: 2 * time.Minute, probed: true, unknownIP: true, wantDue: true},
		{name: "null status alone does not shorten repeat cooldown", age: 2 * time.Minute, probed: true, transportOK: true},
		{name: "interrupted routine probe retains cooldown", age: 2 * time.Minute, mode: "normal"},
		{name: "interrupted 429 keeps rate limit floor", age: 2 * time.Minute, status: 429, probed: true},
		{name: "interrupted 401 keeps auth retry floor", age: 2 * time.Minute, status: 401, probed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newProbePostgres(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			exitIP := "198.51.100.25"
			if tc.unknownIP {
				exitIP = ""
			}
			_, err := db.Exec(`INSERT INTO proxies(id, exit_ip) VALUES (3, $1), (4, $1)`, exitIP)
			require.NoError(t, err)
			oldProxy := int64(3)
			if tc.unknownIP {
				oldProxy = 4
			}
			_, err = db.Exec(`
				INSERT INTO accounts(id, proxy_id, updated_at, deleted_at)
				VALUES (70, $1, $2, $2), (71, 4, $2, NULL)
			`, oldProxy, now)
			require.NoError(t, err)
			_, err = db.Exec(`
				INSERT INTO openai_downgrade_probe_results(account_id, proxy_id, mode, http_status, created_at, transport_ok, answer_correct)
				VALUES (70, $1, 'normal', NULLIF($2, 0), $3, $4, CASE WHEN $4 THEN TRUE ELSE NULL END)
			`, oldProxy, tc.status, now.Add(-tc.age), tc.transportOK)
			require.NoError(t, err)
			repo := &openAIDowngradeProbeRepository{db: db}
			proxyID := int64(4)
			state, err := repo.EnsureOpenAIDowngradeState(t.Context(), 71, &proxyID, now.Add(-time.Hour))
			require.NoError(t, err)
			state.ProbeMode = "qualification"
			if tc.mode != "" {
				state.ProbeMode = tc.mode
			}
			if tc.probed {
				lastProbeAt := now.Add(-time.Hour)
				state.LastProbeAt = &lastProbeAt
			}
			require.NoError(t, repo.SaveOpenAIDowngradeState(t.Context(), state))
			due, err := repo.ListDueOpenAIDowngradeStates(t.Context(), now, 100)
			require.NoError(t, err)
			if tc.wantDue {
				require.Len(t, due, 1)
				require.EqualValues(t, 71, due[0].AccountID)
			} else {
				require.Empty(t, due)
			}
			var historyCount int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM openai_downgrade_probe_results`).Scan(&historyCount))
			require.Equal(t, 1, historyCount, "speedup must not erase audit history")
		})
	}
}

func TestOpenAIProbeQualificationTransportRetryPostgres(t *testing.T) {
	db := newProbePostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.Exec(`INSERT INTO proxies(id, exit_ip) VALUES (3, '198.51.100.25'), (4, '198.51.100.25')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO accounts(id, proxy_id, updated_at, schedulable)
		VALUES (70, 3, $1, TRUE), (71, 4, $1, FALSE), (72, 4, $1, FALSE)`, now)
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	for _, id := range []int64{70, 71, 72} {
		proxyID := int64(4)
		if id == 70 {
			proxyID = 3
		}
		state, err := repo.EnsureOpenAIDowngradeState(t.Context(), id, &proxyID, now.Add(-time.Hour))
		require.NoError(t, err)
		if id != 70 {
			state.ProbeMode = "qualification"
			state.NextProbeAt = now.Add(-time.Minute)
		}
		if id == 71 {
			lastProbe := now.Add(-2 * time.Minute)
			state.LastProbeAt = &lastProbe
			state.NextProbeAt = now.Add(-30 * time.Minute)
		}
		require.NoError(t, repo.SaveOpenAIDowngradeState(t.Context(), state))
	}
	_, err = db.Exec(`INSERT INTO openai_downgrade_probe_results
		(account_id, proxy_id, mode, http_status, transport_ok, created_at)
		VALUES (71, 4, 'qualification', NULL, FALSE, $1)`, now.Add(-2*time.Minute))
	require.NoError(t, err)
	due, err := repo.ListDueOpenAIDowngradeStates(t.Context(), now, 100)
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.EqualValues(t, 71, due[0].AccountID, "oldest admission retry precedes a new admission and routine polling")
	var schedulable bool
	require.NoError(t, db.QueryRow(`SELECT schedulable FROM accounts WHERE id=71`).Scan(&schedulable))
	require.False(t, schedulable, "selecting a retry must never enable an unqualified account")

	_, err = db.Exec(`INSERT INTO openai_downgrade_probe_results
		(account_id, proxy_id, mode, http_status, transport_ok, created_at)
		VALUES (70, 3, 'normal', 429, FALSE, $1)`, now.Add(-2*time.Minute))
	require.NoError(t, err)
	due, err = repo.ListDueOpenAIDowngradeStates(t.Context(), now, 100)
	require.NoError(t, err)
	require.Empty(t, due, "a transport retry must not bypass another proxy's same-exit 429")
}

func TestOpenAIProbeImportPriorityPostgres(t *testing.T) {
	db := newProbePostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := db.Exec(`
		INSERT INTO proxies(id, exit_ip) VALUES (3, '198.51.100.25'), (4, '198.51.100.26');
	`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO accounts(id, proxy_id, updated_at)
		VALUES (70, 3, $1), (71, 3, $1), (72, 4, $1), (73, 3, $1)`, now)
	require.NoError(t, err)
	repo := &openAIDowngradeProbeRepository{db: db}
	for _, id := range []int64{70, 71, 72, 73} {
		proxyID := int64(3)
		if id == 72 {
			proxyID = 4
		}
		state, err := repo.EnsureOpenAIDowngradeState(t.Context(), id, &proxyID, now.Add(-time.Hour))
		require.NoError(t, err)
		if id == 71 || id == 73 {
			state.ProbeMode = "qualification"
			state.NextProbeAt = now
		}
		require.NoError(t, repo.SaveOpenAIDowngradeState(t.Context(), state))
	}
	due, err := repo.ListDueOpenAIDowngradeStates(t.Context(), now, 100)
	require.NoError(t, err)
	require.Len(t, due, 2, "only one account per exit in a scan")
	require.EqualValues(t, 71, due[0].AccountID, "new admission precedes routine work")
	require.Equal(t, service.OpenAIDowngradeStateOnDuty, due[0].State)
	require.EqualValues(t, 72, due[1].AccountID)
}
