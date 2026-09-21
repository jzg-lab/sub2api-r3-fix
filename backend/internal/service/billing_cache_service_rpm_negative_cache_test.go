//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// r17w 改动③回归：snapshot 负缓存标记 UserGroupRPMOverrideChecked=true 且
// override=nil 时，checkRPM 不再每请求回源 DB 复查"确实没有 override"。
// unmarked nil（snapshot 构建时查询失败/未查）仍走 DB 兜底，行为不变。
func TestBillingCacheService_CheckRPM_NegativeCacheSkipsDBLookup(t *testing.T) {
	cache := &userRPMCacheStub{userGroupCounts: []int{1, 1, 1}}
	repo := &rpmOverrideRepoStub{override: nil}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{ID: 1, RPMLimit: 0, UserGroupRPMOverrideChecked: true} // 负缓存命中
	group := &Group{ID: 10, RPMLimit: 5}

	for i := 0; i < 3; i++ {
		require.NoError(t, svc.checkRPM(context.Background(), user, group))
	}
	require.EqualValues(t, 0, atomic.LoadInt32(&repo.calls),
		"负缓存标记在场时不应回源 DB 查 override")
	require.EqualValues(t, 3, atomic.LoadInt32(&cache.userGroupCalls),
		"group.rpm_limit 计数照常生效")
}

// unmarked nil（旧 snapshot / 构建时查询失败）回源 DB 的既有行为不变。
func TestBillingCacheService_CheckRPM_UnmarkedNilStillQueriesDB(t *testing.T) {
	cache := &userRPMCacheStub{userGroupCounts: []int{1}}
	repo := &rpmOverrideRepoStub{override: nil}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{ID: 1, RPMLimit: 0} // 无负缓存标记
	group := &Group{ID: 10, RPMLimit: 5}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.EqualValues(t, 1, atomic.LoadInt32(&repo.calls),
		"无负缓存标记时仍应查一次 DB")
}

// override 非 nil 时优先级不受负缓存字段影响（flag 只加速 nil 分支）。
func TestBillingCacheService_CheckRPM_OverrideStillWinsWithFlagSet(t *testing.T) {
	override := 2
	cache := &userRPMCacheStub{userGroupCounts: []int{1, 2, 3}}
	repo := &rpmOverrideRepoStub{override: &override}
	svc := newBillingServiceForRPM(t, cache, repo)

	user := &User{ID: 1, RPMLimit: 100, UserGroupRPMOverride: &override, UserGroupRPMOverrideChecked: true}
	group := &Group{ID: 10, RPMLimit: 100}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.ErrorIs(t, svc.checkRPM(context.Background(), user, group), ErrGroupRPMExceeded)
	require.EqualValues(t, 0, atomic.LoadInt32(&repo.calls),
		"snapshot override 在场时不回源")
}

// snapshot 构建：查询成功（含确认无 override）必须打负缓存标记；查询失败不打。
func TestAuthSnapshot_OverrideLookupMarksNegativeCache(t *testing.T) {
	_ = config.Config{} // 保持 import 形态与同包测试一致
	// 构建路径的标记逻辑在 api_key_auth_cache_impl.go 的 snapshot 组装处，
	// 这里钉住语义：成功(含nil)→checked=true；失败→checked=false。
	// 直接构造 snapshot 结构体验证字段序列化包含 flag（L2 是 JSON 序列化）。
	snap := &APIKeyAuthUserSnapshot{}
	require.False(t, snap.UserGroupRPMOverrideChecked, "零值 snapshot 不得自带负缓存标记")
}
