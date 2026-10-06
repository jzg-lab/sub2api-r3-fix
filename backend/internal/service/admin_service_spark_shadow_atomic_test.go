//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type shadowAtomicBoundaryRepo struct {
	*sparkShadowRepoStub
	atomicErr   error
	atomicCalls int
	legacyCalls int
	groups      []AccountGroup
}

func (r *shadowAtomicBoundaryRepo) Create(context.Context, *Account) error {
	r.legacyCalls++
	return errors.New("non-transactional shadow create must not be called")
}

func (r *shadowAtomicBoundaryRepo) BindGroups(context.Context, int64, []int64) error {
	r.legacyCalls++
	return errors.New("non-transactional shadow group binding must not be called")
}

func (r *shadowAtomicBoundaryRepo) Delete(context.Context, int64) error {
	r.legacyCalls++
	return errors.New("compensating shadow delete must not be called")
}

func (r *shadowAtomicBoundaryRepo) CreateWithAccountGroups(ctx context.Context, account *Account, groups []AccountGroup) error {
	r.atomicCalls++
	r.groups = append([]AccountGroup(nil), groups...)
	if r.atomicErr != nil {
		return r.atomicErr
	}
	return r.sparkShadowRepoStub.CreateWithAccountGroups(ctx, account, groups)
}

func newShadowAtomicBoundaryFixture(t *testing.T) (*shadowAtomicBoundaryRepo, *Account, *adminServiceImpl) {
	t.Helper()
	base := newSparkShadowRepoStub()
	parent := &Account{
		Name: "parent", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Concurrency: 100,
	}
	require.NoError(t, base.Create(context.Background(), parent))
	repo := &shadowAtomicBoundaryRepo{sparkShadowRepoStub: base}
	return repo, parent, &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
}

func TestCreateShadowAtomicBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parent   []int64
		explicit []int64
		want     []int64
	}{
		{name: "ungrouped"},
		{name: "inherit", parent: []int64{7, 4}, want: []int64{7, 4}},
		{name: "explicit override", parent: []int64{7, 4}, explicit: []int64{9, 3}, want: []int64{9, 3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, parent, svc := newShadowAtomicBoundaryFixture(t)
			parent.GroupIDs = tc.parent
			require.NoError(t, repo.sparkShadowRepoStub.Update(context.Background(), parent))
			beforeParent := append([]int64(nil), tc.parent...)
			beforeExplicit := append([]int64(nil), tc.explicit...)
			shadow, err := svc.CreateShadow(context.Background(), parent.ID, ShadowOptions{GroupIDs: tc.explicit})
			require.NoError(t, err)
			require.Equal(t, 1, repo.atomicCalls)
			require.Zero(t, repo.legacyCalls)
			require.Equal(t, parent.Concurrency, shadow.Concurrency)
			require.Equal(t, parent.ID, *shadow.ParentAccountID)
			require.Equal(t, QuotaDimensionSpark, shadow.QuotaDimension)
			require.Len(t, shadow.GroupIDs, len(tc.want))
			require.Len(t, shadow.AccountGroups, len(tc.want))
			for i, id := range tc.want {
				require.Equal(t, id, shadow.GroupIDs[i])
				require.Equal(t, AccountGroup{GroupID: id, Priority: i + 1}, repo.groups[i])
				require.Equal(t, AccountGroup{AccountID: shadow.ID, GroupID: id, Priority: i + 1}, shadow.AccountGroups[i])
			}
			require.Equal(t, beforeParent, parent.GroupIDs)
			require.Equal(t, beforeExplicit, tc.explicit)
		})
	}
}

func TestCreateShadowAtomicFailureDoesNotPublishOrCompensate(t *testing.T) {
	for _, writeErr := range []error{errors.New("group write failed"), errors.New("outbox failed"), errors.New("commit failed"), context.Canceled} {
		t.Run(writeErr.Error(), func(t *testing.T) {
			repo, parent, svc := newShadowAtomicBoundaryFixture(t)
			repo.atomicErr = writeErr
			shadow, err := svc.CreateShadow(context.Background(), parent.ID, ShadowOptions{GroupIDs: []int64{7}})
			require.ErrorIs(t, err, writeErr)
			require.Nil(t, shadow)
			require.Equal(t, 1, repo.atomicCalls)
			require.Zero(t, repo.legacyCalls)
			require.Len(t, repo.accounts, 1)
			require.Empty(t, repo.groupsOf)

			repo.atomicErr = nil
			shadow, err = svc.CreateShadow(context.Background(), parent.ID, ShadowOptions{GroupIDs: []int64{7}})
			require.NoError(t, err)
			require.NotNil(t, shadow)
			require.Len(t, repo.accounts, 2)
			require.Zero(t, repo.legacyCalls)
		})
	}
}

func TestCreateShadowAtomicCancelledContextDoesNotCreate(t *testing.T) {
	repo, parent, svc := newShadowAtomicBoundaryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shadow, err := svc.CreateShadow(ctx, parent.ID, ShadowOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, shadow)
	require.Zero(t, repo.legacyCalls)
	require.Len(t, repo.accounts, 1)
	require.Empty(t, repo.groupsOf)
}

func TestCreateShadowMissingAtomicWriterFailsClosed(t *testing.T) {
	repo, parent, svc := newShadowAtomicBoundaryFixture(t)
	svc.accountDuplicateRepo = nil
	shadow, err := svc.CreateShadow(context.Background(), parent.ID, ShadowOptions{GroupIDs: []int64{7}})
	require.ErrorContains(t, err, "atomic account creation repository is not configured")
	require.Nil(t, shadow)
	require.Zero(t, repo.atomicCalls)
	require.Zero(t, repo.legacyCalls)
	require.Len(t, repo.accounts, 1)
}
