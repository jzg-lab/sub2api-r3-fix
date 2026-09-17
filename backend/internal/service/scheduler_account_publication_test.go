//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type schedulerPublicationRepo struct {
	AccountRepository
	current *Account
	err     error
	reads   int
}

func (r *schedulerPublicationRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.current, r.err
}

func TestSchedulerAccountPublicationConsumersUseCommittedGeneration(t *testing.T) {
	for _, consumer := range []string{"refresh", "snapshot"} {
		t.Run(consumer, func(t *testing.T) {
			oldProxy, newProxy := int64(1), int64(2)
			stale := &Account{ID: 91, Type: AccountTypeOAuth, Schedulable: true, ProxyID: &oldProxy}
			current := &Account{ID: 91, Type: AccountTypeOAuth, Schedulable: false,
				ProxyID: &newProxy, UpdatedAt: time.Now()}
			repo := &schedulerPublicationRepo{current: current}
			cache := &tokenRefreshSchedulerCache{}
			if consumer == "refresh" {
				s := &TokenRefreshService{accountRepo: repo, schedulerCache: cache}
				s.postRefreshStateSync(context.Background(), stale)
			} else {
				s := &SchedulerSnapshotService{accountRepo: repo, cache: cache}
				require.NoError(t, s.UpdateAccountInCache(context.Background(), stale))
			}
			require.Equal(t, 1, repo.reads)
			require.Equal(t, 1, cache.setAccountCalls)
			require.False(t, cache.lastAccount.Schedulable)
			require.Equal(t, newProxy, *cache.lastAccount.ProxyID)
			require.Equal(t, current.UpdatedAt, cache.lastAccount.UpdatedAt)
			require.True(t, stale.Schedulable, "the request snapshot remains owned by its caller")
		})
	}
}

func TestSchedulerAccountPublicationConsumersDoNotWriteOnReloadFailure(t *testing.T) {
	for _, readError := range []error{nil, ErrAccountNotFound, errors.New("database unavailable")} {
		t.Run(map[bool]string{true: "error", false: "nil"}[readError != nil], func(t *testing.T) {
			repo := &schedulerPublicationRepo{err: readError}
			cache := &tokenRefreshSchedulerCache{}
			stale := &Account{ID: 92, Type: AccountTypeOAuth, Schedulable: true}
			refresh := &TokenRefreshService{accountRepo: repo, schedulerCache: cache}
			refresh.postRefreshStateSync(context.Background(), stale)
			snapshot := &SchedulerSnapshotService{accountRepo: repo, cache: cache}
			require.Error(t, snapshot.UpdateAccountInCache(context.Background(), stale))
			require.Zero(t, cache.setAccountCalls)
		})
	}
}
