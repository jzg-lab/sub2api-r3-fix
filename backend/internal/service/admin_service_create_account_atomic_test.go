package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func (r *upstreamBillingProbeAccountRepo) CreateWithAccountGroups(ctx context.Context, account *Account, groups []AccountGroup) error {
	if len(groups) != 0 {
		return errors.New("billing probe fixture expects an ungrouped account")
	}
	return r.Create(ctx, account)
}

type atomicAccountCreateTestRepo struct {
	AccountRepository
	err         error
	calls       int
	groups      []AccountGroup
	legacyCalls int
}

func (r *atomicAccountCreateTestRepo) Create(context.Context, *Account) error {
	r.legacyCalls++
	return errors.New("non-transactional create must not be called")
}

func (r *atomicAccountCreateTestRepo) BindGroups(context.Context, int64, []int64) error {
	r.legacyCalls++
	return errors.New("non-transactional bind must not be called")
}

func (r *atomicAccountCreateTestRepo) CreateWithAccountGroups(_ context.Context, account *Account, groups []AccountGroup) error {
	r.calls++
	r.groups = append([]AccountGroup(nil), groups...)
	if r.err != nil {
		return r.err
	}
	account.ID = 71
	account.GroupIDs = make([]int64, len(groups))
	account.AccountGroups = append([]AccountGroup(nil), groups...)
	for i := range account.AccountGroups {
		account.AccountGroups[i].AccountID = account.ID
		account.GroupIDs[i] = groups[i].GroupID
	}
	return nil
}

func atomicAccountCreateInput(groupIDs []int64) *CreateAccountInput {
	return &CreateAccountInput{
		Name:                  "atomic-create",
		Platform:              PlatformOpenAI,
		Type:                  AccountTypeAPIKey,
		GroupIDs:              groupIDs,
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	}
}

func TestAdminCreateAccountUsesAtomicGroupAndOutboxWriter(t *testing.T) {
	for _, groupIDs := range [][]int64{nil, {9, 3}} {
		repo := &atomicAccountCreateTestRepo{}
		svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
		account, err := svc.CreateAccount(context.Background(), atomicAccountCreateInput(groupIDs))
		require.NoError(t, err)
		require.Equal(t, 1, repo.calls)
		require.Zero(t, repo.legacyCalls)
		require.Equal(t, int64(71), account.ID)
		require.Equal(t, len(groupIDs), len(account.GroupIDs))
		for i, id := range groupIDs {
			require.Equal(t, id, account.GroupIDs[i])
			require.Equal(t, AccountGroup{GroupID: id, Priority: i + 1}, repo.groups[i])
			require.Equal(t, account.ID, account.AccountGroups[i].AccountID)
		}
	}
}

func TestAdminCreateAccountDoesNotPublishFailedAtomicWrite(t *testing.T) {
	writeErr := errors.New("group or outbox write failed")
	repo := &atomicAccountCreateTestRepo{err: writeErr}
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	account, err := svc.CreateAccount(context.Background(), atomicAccountCreateInput([]int64{9}))
	require.ErrorIs(t, err, writeErr)
	require.Nil(t, account)
	require.Equal(t, 1, repo.calls)
	require.Zero(t, repo.legacyCalls)
}

func TestAdminCreateAccountMissingAtomicWriterFailsClosed(t *testing.T) {
	repo := &atomicAccountCreateTestRepo{}
	svc := &adminServiceImpl{accountRepo: repo}
	account, err := svc.CreateAccount(context.Background(), atomicAccountCreateInput(nil))
	require.ErrorContains(t, err, "atomic account creation repository is not configured")
	require.Nil(t, account)
	require.Zero(t, repo.calls)
	require.Zero(t, repo.legacyCalls)
}

func TestAdminCreateAccountNilInputDoesNotWrite(t *testing.T) {
	repo := &atomicAccountCreateTestRepo{}
	svc := &adminServiceImpl{accountRepo: repo, accountDuplicateRepo: repo}
	account, err := svc.CreateAccount(context.Background(), nil)
	require.ErrorIs(t, err, ErrAccountNilInput)
	require.Nil(t, account)
	require.Zero(t, repo.calls)
	require.Zero(t, repo.legacyCalls)
}
