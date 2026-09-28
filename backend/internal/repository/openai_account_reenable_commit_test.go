package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type reenableAccountRow struct {
	updatedAt       time.Time
	proxyID         any
	status          string
	schedulable     bool
	platform        string
	accountType     string
	parentAccountID any
	notExpired      bool
	errorMessage    any
}

func reenableCommitFixture() *service.OpenAIAccountReenableMutation {
	now := time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC)
	proxyID := int64(3)
	return &service.OpenAIAccountReenableMutation{
		AccountID:                7,
		ExpectedAccountUpdatedAt: now.Add(-2 * time.Hour),
		ExpectedStateUpdatedAt:   now.Add(-time.Hour),
		ExpectedProxyID:          &proxyID,
		ExpectedStatus:           service.StatusActive,
		ExpectedSchedulable:      false,
		ReenabledAt:              now,
		State: &service.OpenAIDowngradeProbeState{
			AccountID:            7,
			State:                service.OpenAIDowngradeStateOnDuty,
			ProbeMode:            "qualification",
			CurrentProxyID:       &proxyID,
			OriginalProxyID:      &proxyID,
			ConsecutiveFailures:  1,
			NextProbeAt:          now.Add(5 * time.Minute),
			UpdatedAt:            now,
			ConsecutiveSuccesses: 0,
		},
	}
}

func validReenableAccountRow(mutation *service.OpenAIAccountReenableMutation) reenableAccountRow {
	var proxyID any
	if mutation.ExpectedProxyID != nil {
		proxyID = *mutation.ExpectedProxyID
	}
	return reenableAccountRow{
		updatedAt:       mutation.ExpectedAccountUpdatedAt,
		proxyID:         proxyID,
		status:          mutation.ExpectedStatus,
		schedulable:     mutation.ExpectedSchedulable,
		platform:        service.PlatformOpenAI,
		accountType:     service.AccountTypeOAuth,
		parentAccountID: nil,
		notExpired:      true,
		errorMessage:    nil,
	}
}

func expectReenableAccountLock(
	mock sqlmock.Sqlmock,
	mutation *service.OpenAIAccountReenableMutation,
	row reenableAccountRow,
) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT updated_at, proxy_id, status, schedulable, platform, type,.*FROM accounts.*FOR UPDATE`).
		WithArgs(mutation.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{
			"updated_at", "proxy_id", "status", "schedulable", "platform", "type",
			"parent_account_id", "not_expired", "error_message",
		}).AddRow(
			row.updatedAt, row.proxyID, row.status, row.schedulable, row.platform,
			row.accountType, row.parentAccountID, row.notExpired, row.errorMessage,
		))
}

func expectReenableControlLock(mock sqlmock.Sqlmock, accountID int64, paused bool, ownedError any) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT manual_paused, owned_error.*FROM openai_downgrade_probe_controls.*FOR UPDATE`).
		WithArgs(accountID).
		WillReturnRows(sqlmock.NewRows([]string{"manual_paused", "owned_error"}).AddRow(paused, ownedError))
}

func expectReenableStateLock(
	mock sqlmock.Sqlmock,
	mutation *service.OpenAIAccountReenableMutation,
	updatedAt time.Time,
	state string,
) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT updated_at, state.*FROM openai_downgrade_probe_states.*FOR UPDATE`).
		WithArgs(mutation.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at", "state"}).AddRow(updatedAt, state))
}

func TestOpenAIAccountReenableCommitSuccessIsAtomic(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "already_unpaused", true: "clear_pause"}[paused], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			mutation.Unpause = paused
			mock.ExpectBegin()
			expectReenableAccountLock(mock, mutation, validReenableAccountRow(mutation))
			expectReenableControlLock(mock, mutation.AccountID, paused, nil)
			expectReenableStateLock(mock, mutation, mutation.ExpectedStateUpdatedAt, service.OpenAIDowngradeStatePendingReplace)
			if paused {
				mock.ExpectExec(`UPDATE openai_downgrade_probe_controls`).
					WithArgs(mutation.AccountID).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
					WillReturnResult(sqlmock.NewResult(0, 1))
			}
			mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`UPDATE openai_downgrade_probe_states`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			repo := &openAIDowngradeProbeRepository{db: db}
			unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.NoError(t, err)
			require.Equal(t, paused, unpaused)
			require.True(t, mutation.State.UpdatedAt.After(mutation.ExpectedStateUpdatedAt))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIAccountReenableCommitRollsBackAtEveryBoundary(t *testing.T) {
	injected := errors.New("injected reenable persistence failure")
	for _, failure := range []string{
		"begin", "account_lock", "control_lock", "state_lock", "clear_pause",
		"unpause_event", "reenable_event", "state_write", "commit",
	} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			mutation.Unpause = true
			before := *mutation.State
			begin := mock.ExpectBegin()
			if failure == "begin" {
				begin.WillReturnError(injected)
			} else {
				accountLock := mock.ExpectQuery(`(?s)SELECT updated_at, proxy_id, status, schedulable, platform, type,.*FROM accounts.*FOR UPDATE`).
					WithArgs(mutation.AccountID)
				if failure == "account_lock" {
					accountLock.WillReturnError(injected)
				} else {
					row := validReenableAccountRow(mutation)
					accountLock.WillReturnRows(sqlmock.NewRows([]string{
						"updated_at", "proxy_id", "status", "schedulable", "platform", "type",
						"parent_account_id", "not_expired", "error_message",
					}).AddRow(
						row.updatedAt, row.proxyID, row.status, row.schedulable, row.platform,
						row.accountType, row.parentAccountID, row.notExpired, row.errorMessage,
					))

					controlLock := mock.ExpectQuery(`(?s)SELECT manual_paused, owned_error.*FROM openai_downgrade_probe_controls.*FOR UPDATE`).
						WithArgs(mutation.AccountID)
					if failure == "control_lock" {
						controlLock.WillReturnError(injected)
					} else {
						controlLock.WillReturnRows(sqlmock.NewRows([]string{"manual_paused", "owned_error"}).AddRow(true, nil))

						stateLock := mock.ExpectQuery(`(?s)SELECT updated_at, state.*FROM openai_downgrade_probe_states.*FOR UPDATE`).
							WithArgs(mutation.AccountID)
						if failure == "state_lock" {
							stateLock.WillReturnError(injected)
						} else {
							stateLock.WillReturnRows(sqlmock.NewRows([]string{"updated_at", "state"}).
								AddRow(mutation.ExpectedStateUpdatedAt, service.OpenAIDowngradeStatePendingReplace))

							writes := []struct {
								name  string
								query string
							}{
								{name: "clear_pause", query: `UPDATE openai_downgrade_probe_controls`},
								{name: "unpause_event", query: `INSERT INTO openai_downgrade_probe_events`},
								{name: "reenable_event", query: `INSERT INTO openai_downgrade_probe_events`},
								{name: "state_write", query: `UPDATE openai_downgrade_probe_states`},
							}
							for _, write := range writes {
								expect := mock.ExpectExec(write.query)
								if write.name == failure {
									expect.WillReturnError(injected)
									break
								}
								expect.WillReturnResult(sqlmock.NewResult(0, 1))
							}
							if failure == "commit" {
								mock.ExpectCommit().WillReturnError(injected)
							}
						}
					}
				}
				if failure != "commit" {
					mock.ExpectRollback()
				}
			}

			repo := &openAIDowngradeProbeRepository{db: db}
			_, err = repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.ErrorIs(t, err, injected)
			require.Equal(t, before, *mutation.State, "failed transaction must not publish a new state generation")
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIAccountReenableCommitRejectsStaleGeneration(t *testing.T) {
	for _, changed := range []string{"account", "state", "proxy_added", "proxy_removed", "proxy_changed", "status", "schedulable"} {
		t.Run(changed, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			if changed == "proxy_added" {
				mutation.ExpectedProxyID = nil
			}
			before := *mutation.State
			mock.ExpectBegin()
			accountRow := validReenableAccountRow(mutation)
			switch changed {
			case "account":
				accountRow.updatedAt = accountRow.updatedAt.Add(time.Second)
			case "proxy_added", "proxy_changed":
				accountRow.proxyID = int64(4)
			case "proxy_removed":
				accountRow.proxyID = nil
			case "status":
				accountRow.status = service.StatusDisabled
			case "schedulable":
				accountRow.schedulable = !mutation.ExpectedSchedulable
			}
			expectReenableAccountLock(mock, mutation, accountRow)
			if changed == "state" {
				expectReenableControlLock(mock, mutation.AccountID, false, nil)
				expectReenableStateLock(
					mock, mutation, mutation.ExpectedStateUpdatedAt.Add(time.Second),
					service.OpenAIDowngradeStatePendingReplace,
				)
			}
			mock.ExpectRollback()

			repo := &openAIDowngradeProbeRepository{db: db}
			_, err = repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
			require.Equal(t, before, *mutation.State)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIAccountReenableCommitAcceptsNoProxyOrControlRow(t *testing.T) {
	for _, missingControl := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing_control", true: "missing_control"}[missingControl], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			mutation.ExpectedProxyID = nil
			mutation.State.CurrentProxyID = nil
			mutation.State.OriginalProxyID = nil
			mock.ExpectBegin()
			expectReenableAccountLock(mock, mutation, validReenableAccountRow(mutation))
			control := expectReenableControlLock(mock, mutation.AccountID, false, nil)
			if missingControl {
				control.WillReturnError(sql.ErrNoRows)
			}
			expectReenableStateLock(mock, mutation, mutation.ExpectedStateUpdatedAt, service.OpenAIDowngradeStatePendingReplace)
			mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
				WithArgs(mutation.AccountID, nil, "manual_reenable", sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`UPDATE openai_downgrade_probe_states`).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			repo := &openAIDowngradeProbeRepository{db: db}
			unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.NoError(t, err)
			require.False(t, unpaused)
			require.Nil(t, mutation.State.CurrentProxyID)
			require.True(t, mutation.State.UpdatedAt.After(mutation.ExpectedStateUpdatedAt))
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIAccountReenableCommitRejectsLostWrites(t *testing.T) {
	for _, boundary := range []string{"clear_pause", "state_write"} {
		for _, rowCount := range []int64{0, 2} {
			t.Run(boundary+"/"+map[int64]string{0: "no_rows", 2: "multiple_rows"}[rowCount], func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()

				mutation := reenableCommitFixture()
				mutation.Unpause = true
				before := *mutation.State
				mock.ExpectBegin()
				expectReenableAccountLock(mock, mutation, validReenableAccountRow(mutation))
				expectReenableControlLock(mock, mutation.AccountID, true, nil)
				expectReenableStateLock(mock, mutation, mutation.ExpectedStateUpdatedAt, service.OpenAIDowngradeStatePendingReplace)
				clearRows := int64(1)
				if boundary == "clear_pause" {
					clearRows = rowCount
				}
				mock.ExpectExec(`UPDATE openai_downgrade_probe_controls`).
					WithArgs(mutation.AccountID).
					WillReturnResult(sqlmock.NewResult(0, clearRows))
				if boundary == "state_write" {
					mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
						WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
						WillReturnResult(sqlmock.NewResult(0, 1))
					mock.ExpectExec(`UPDATE openai_downgrade_probe_states`).
						WillReturnResult(sqlmock.NewResult(0, rowCount))
				}
				mock.ExpectRollback()

				repo := &openAIDowngradeProbeRepository{db: db}
				unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

				require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
				require.False(t, unpaused)
				require.Equal(t, before, *mutation.State)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestOpenAIAccountReenableCommitRejectsPausedAndIneligibleAccounts(t *testing.T) {
	tests := []struct {
		name       string
		account    func(*service.OpenAIAccountReenableMutation) reenableAccountRow
		paused     bool
		ownedError any
		unpause    bool
		wantErr    error
	}{
		{
			name: "manual_pause_without_unpause",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				return validReenableAccountRow(m)
			},
			paused: true, wantErr: service.ErrOpenAIReenablePaused,
		},
		{
			name: "schedulable_would_bypass_qualification",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.schedulable = true
				m.ExpectedSchedulable = true
				return row
			},
			wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "wrong_platform",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.platform = service.PlatformAnthropic
				return row
			},
			wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "non_oauth",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.accountType = service.AccountTypeAPIKey
				return row
			},
			wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "shadow_account",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.parentAccountID = int64(99)
				return row
			},
			wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "expired",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.notExpired = false
				return row
			},
			wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "unowned_error",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				row := validReenableAccountRow(m)
				row.status = service.StatusError
				row.errorMessage = "account error"
				m.ExpectedStatus = service.StatusError
				return row
			},
			ownedError: "different error", wantErr: service.ErrOpenAIReenableBlocked,
		},
		{
			name: "state_already_reenabled",
			account: func(m *service.OpenAIAccountReenableMutation) reenableAccountRow {
				return validReenableAccountRow(m)
			},
			wantErr: service.ErrOpenAIReenableNotDead,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			mutation.Unpause = tt.unpause
			before := *mutation.State
			accountRow := tt.account(mutation)
			mock.ExpectBegin()
			expectReenableAccountLock(mock, mutation, accountRow)
			expectReenableControlLock(mock, mutation.AccountID, tt.paused, tt.ownedError)
			if tt.wantErr == service.ErrOpenAIReenableNotDead {
				expectReenableStateLock(
					mock, mutation, mutation.ExpectedStateUpdatedAt,
					service.OpenAIDowngradeStateOnDuty,
				)
			}
			mock.ExpectRollback()

			repo := &openAIDowngradeProbeRepository{db: db}
			_, err = repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.ErrorIs(t, err, tt.wantErr)
			require.Equal(t, before, *mutation.State)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestOpenAIAccountReenableCommitAcceptsOwnedError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mutation := reenableCommitFixture()
	mutation.ExpectedStatus = service.StatusError
	accountRow := validReenableAccountRow(mutation)
	accountRow.status = service.StatusError
	accountRow.errorMessage = "owned failure"
	mock.ExpectBegin()
	expectReenableAccountLock(mock, mutation, accountRow)
	expectReenableControlLock(mock, mutation.AccountID, false, "owned failure")
	expectReenableStateLock(mock, mutation, mutation.ExpectedStateUpdatedAt, service.OpenAIDowngradeStatePendingReplace)
	mock.ExpectExec(`INSERT INTO openai_downgrade_probe_events`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE openai_downgrade_probe_states`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &openAIDowngradeProbeRepository{db: db}
	unpaused, err := repo.CommitOpenAIAccountReenable(context.Background(), mutation)

	require.NoError(t, err)
	require.False(t, unpaused)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenAIAccountReenableCommitRejectsMissingRowsAsStale(t *testing.T) {
	for _, missing := range []string{"account", "state"} {
		t.Run(missing, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()

			mutation := reenableCommitFixture()
			mock.ExpectBegin()
			if missing == "account" {
				mock.ExpectQuery(`(?s)SELECT updated_at, proxy_id, status, schedulable, platform, type,.*FROM accounts.*FOR UPDATE`).
					WithArgs(mutation.AccountID).
					WillReturnError(sql.ErrNoRows)
			} else {
				expectReenableAccountLock(mock, mutation, validReenableAccountRow(mutation))
				expectReenableControlLock(mock, mutation.AccountID, false, nil)
				mock.ExpectQuery(`(?s)SELECT updated_at, state.*FROM openai_downgrade_probe_states.*FOR UPDATE`).
					WithArgs(mutation.AccountID).
					WillReturnError(sql.ErrNoRows)
			}
			mock.ExpectRollback()

			repo := &openAIDowngradeProbeRepository{db: db}
			_, err = repo.CommitOpenAIAccountReenable(context.Background(), mutation)

			require.ErrorIs(t, err, service.ErrOpenAIProbeStale)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
