package repository

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIProbeImportCooldownPostgres(t *testing.T) {
	for _, tc := range []struct {
		name      string
		age       time.Duration
		status    int
		probed    bool
		mode      string
		unknownIP bool
		wantDue   bool
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
				INSERT INTO openai_downgrade_probe_results(account_id, proxy_id, mode, http_status, created_at)
				VALUES (70, $1, 'normal', $2, $3)
			`, oldProxy, tc.status, now.Add(-tc.age))
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
