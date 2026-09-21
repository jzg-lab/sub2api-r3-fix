package service

// 选号 DB recheck 的请求内去重(r17x A 项)。
//
// 背景:selectAccountWithSchedulerOnce 的能力重试循环会带着扩大的排除集整轮
// 重调 load-awareness,Layer1(sticky)/LoadMap/Layer3 三层循环也会对同一账号
// 重复 recheck——同一账号在单次选号内被 GetByID 回源多次,而单次选号是 ms 级
// 窗口、过滤器参数恒定,第二次及以后的 DB 回源几乎必然返回同一行。
//
// 方案:recheck 结果按(账号 ID, requireCompact)在选号调用作用域内 memo 化。
// memo 挂在 selectAccountWithSchedulerInGroup 的 ctx 上(每 hop/每轮重试新建,
// 互不共享)。键必须含 requireCompact:selectBestAccount 的 recheck 调用点故意
// 传 false(compact 分级由调用方自行处理),而 load-awareness 循环传真实值——
// 两种形态的过滤器语义不同,不能互串。
//
// 只 memo 确定性结果:
//   - GetByID 成功且通过全部过滤器 → 缓存 *Account(命中直接复用,跳过 DB);
//   - GetByID 成功但被任一过滤器否决 → 缓存 nil(本选号作用域内恒不可用);
//   - GetByID 出错/返回空 → 不缓存(瞬时 DB 抖动,下次照旧回源重试)。
//
// 等价性论证(为何这不改变调度行为):
//  1. 单次选号调用内,同键(账号+requireCompact)的其余过滤器参数
//     (group/platform/model/capability)全部恒定——命中返回旧结果与重放
//     过滤器等价。
//  2. 账号状态变更(SetError/SetRateLimited/探针 commit 等)传播到本进程
//     缓存有 ms 级窗口;旧代码逐候选回源 DB 消费的同样是这个窗口内的数据,
//     memo 只是把"窗口内重复读同一行"合并为"窗口内读一次"。
//  3. 粘性删除副作用(deleteStickySessionAccountID)发生在 recheck 返回 nil
//     的调用点上层,memo 返回 nil 的路径照旧触发该副作用。
//  4. 瞬时 DB 错误不缓存:故障降级行为与旧代码一致。
//
// map 仅单 goroutine 读写(选号全程同步调用链),无需加锁。

import "context"

// openAIRecheckMemoKey 是选号作用域 recheck memo 的 ctx 键类型。
type openAIRecheckMemoKey struct{}

// openAIRecheckMemoKeyEntry 是 memo 的复合键:账号 ID + requireCompact 形态。
type openAIRecheckMemoKeyEntry struct {
	accountID     int64
	requireCompact bool
}

// withOpenAIRecheckMemo 在 ctx 上挂一个空的 recheck memo,返回新 ctx。
// 每次选号调用(selectAccountWithSchedulerInGroup 每 hop)新建,请求间/重试间
// 不共享。ctx 无 memo 的调用点(单测/非选号入口)自动退回无 memo 行为。
func withOpenAIRecheckMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAIRecheckMemoKey{}, &openAIRecheckMemo{})
}

// lookupOpenAIRecheckMemo 从 ctx 取 memo;不存在返回 nil。
func lookupOpenAIRecheckMemo(ctx context.Context) *openAIRecheckMemo {
	memo, _ := ctx.Value(openAIRecheckMemoKey{}).(*openAIRecheckMemo)
	return memo
}

// openAIRecheckMemo 记录本选号作用域内已 recheck 过的账号。
type openAIRecheckMemo struct {
	entries map[openAIRecheckMemoKeyEntry]*Account
}

// get 返回 memo 命中结果(nil 可能是"缓存了否决")。ok=false 表示未命中。
func (m *openAIRecheckMemo) get(accountID int64, requireCompact bool) (*Account, bool) {
	if m == nil || m.entries == nil {
		return nil, false
	}
	account, ok := m.entries[openAIRecheckMemoKeyEntry{accountID: accountID, requireCompact: requireCompact}]
	return account, ok
}

// put 记录一次确定性 recheck 结果。
func (m *openAIRecheckMemo) put(accountID int64, requireCompact bool, account *Account) {
	if m == nil {
		return
	}
	if m.entries == nil {
		m.entries = make(map[openAIRecheckMemoKeyEntry]*Account)
	}
	m.entries[openAIRecheckMemoKeyEntry{accountID: accountID, requireCompact: requireCompact}] = account
}
