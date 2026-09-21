//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// r17x A 项回归:选号作用域 recheck memo。
// 同(账号, requireCompact)在单次选号内只回源 DB 一次;
// 不同 requireCompact 形态互不串;无 memo 的 ctx 直通(等价旧代码)。

// recheckMemoCountRepo 统计 GetByID 调用次数的 repo stub。
type recheckMemoCountRepo struct {
	AccountRepository // 嵌入接口:本测试只用 GetByID,其余方法零值不可达
	accounts          map[int64]*Account
	getCalls          map[int64]int
}

func (r *recheckMemoCountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	r.getCalls[id]++
	acc, ok := r.accounts[id]
	if !ok {
		return nil, nil
	}
	return acc, nil
}
func (r *recheckMemoCountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return nil, nil
}
func newRecheckMemoTestService(repo *recheckMemoCountRepo) *OpenAIGatewayService {
	// schedulerSnapshot 非 nil 时 recheck 才走 DB 回源分支。
	return &OpenAIGatewayService{
		accountRepo:       repo,
		schedulerSnapshot: &SchedulerSnapshotService{},
	}
}

func TestOpenAIRecheckMemo_SameKeyQueriesDBOnce(t *testing.T) {
	repo := &recheckMemoCountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}},
		},
		getCalls: map[int64]int{},
	}
	svc := newRecheckMemoTestService(repo)
	ctx := withOpenAIRecheckMemo(context.Background())

	gid := int64(10)
	for i := 0; i < 3; i++ {
		acc := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 1}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
		require.NotNil(t, acc, "健康账号必须通过 recheck")
	}
	require.EqualValues(t, 1, repo.getCalls[1], "同键三次 recheck 只应回源 DB 一次")
}

func TestOpenAIRecheckMemo_VetoedResultCachedAsNil(t *testing.T) {
	repo := &recheckMemoCountRepo{
		accounts: map[int64]*Account{
			2: {ID: 2, Platform: PlatformOpenAI, Status: StatusError, Schedulable: true}, // StatusError 被过滤器否决
		},
		getCalls: map[int64]int{},
	}
	svc := newRecheckMemoTestService(repo)
	ctx := withOpenAIRecheckMemo(context.Background())

	gid := int64(10)
	for i := 0; i < 3; i++ {
		acc := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 2}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
		require.Nil(t, acc, "StatusError 账号必须被否决")
	}
	require.EqualValues(t, 1, repo.getCalls[2], "否决结果 memo 后不得重复回源")
}

func TestOpenAIRecheckMemo_RequireCompactFormsSeparateKeys(t *testing.T) {
	repo := &recheckMemoCountRepo{
		accounts: map[int64]*Account{
			3: {ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}, Extra: map[string]any{"openai_compact_supported": false}}, // 显式不支持=tier 0
		},
		getCalls: map[int64]int{},
	}
	svc := newRecheckMemoTestService(repo)
	ctx := withOpenAIRecheckMemo(context.Background())

	gid := int64(10)
	// requireCompact=true:tier=0 账号被 compact 过滤器否决
	vetoed := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 3}, &gid, PlatformOpenAI, "gpt-5", true, OpenAIEndpointCapabilityChatCompletions)
	require.Nil(t, vetoed, "compact tier=0 + requireCompact=true 必须被否决")
	// requireCompact=false:同账号通过(selectBestAccount 的调用形态)
	passed := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 3}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
	require.NotNil(t, passed, "requireCompact=false 时同账号必须通过")
	require.EqualValues(t, 2, repo.getCalls[3], "不同 requireCompact 形态各自回源,不得互串")
	// 再各来一次:命中 memo,无新增回源
	_ = svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 3}, &gid, PlatformOpenAI, "gpt-5", true, OpenAIEndpointCapabilityChatCompletions)
	_ = svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctx, &Account{ID: 3}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
	require.EqualValues(t, 2, repo.getCalls[3], "同键重复调用必须命中 memo")
}

func TestOpenAIRecheckMemo_NoMemoCtxPassesThrough(t *testing.T) {
	repo := &recheckMemoCountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}},
		},
		getCalls: map[int64]int{},
	}
	svc := newRecheckMemoTestService(repo)

	gid := int64(10)
	for i := 0; i < 3; i++ {
		acc := svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(context.Background(), &Account{ID: 1}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
		require.NotNil(t, acc)
	}
	require.EqualValues(t, 3, repo.getCalls[1], "无 memo 的 ctx(非选号入口)每次都回源,与旧代码一致")
}

func TestOpenAIRecheckMemo_MemoScopesNotShared(t *testing.T) {
	repo := &recheckMemoCountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{10}},
		},
		getCalls: map[int64]int{},
	}
	svc := newRecheckMemoTestService(repo)
	gid := int64(10)

	// 两个独立选号作用域(模拟两个 hop / 两次请求):互不共享
	ctxA := withOpenAIRecheckMemo(context.Background())
	ctxB := withOpenAIRecheckMemo(context.Background())
	_ = svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctxA, &Account{ID: 1}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
	_ = svc.recheckSelectedOpenAIAccountFromDBBeforeProfit(ctxB, &Account{ID: 1}, &gid, PlatformOpenAI, "gpt-5", false, OpenAIEndpointCapabilityChatCompletions)
	require.EqualValues(t, 2, repo.getCalls[1], "不同选号作用域各自回源一次")
}
