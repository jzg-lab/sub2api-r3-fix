package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// =============================================================================
// 自动打票线（相位B 全链，2026-09-21 用户批准：1136 实验号）
//
// 社区对照（upstream-ticket-reference/community-kits/wangyunjeff/sub2api-state-kit
// harvest.go schedule/collect + sx120609 保守注入）：
//   - 社区循环：ticker 每拍扫全部账号 → 无有效票即起采集 job → 动态代理
//     rotate → 采到白名单长度 → 回固定出口复验 → 312 即废票；401/403/429
//     停打；账号级 312 永续 = 放弃（"continuing is what escalated a 312
//     into a wall of 429s"）。
//   - 我们差异（有意）：采集载体=探针顺带（零新增流量形态，不抄官方已自删
//     的 pong 词面针）；调度=复用探针状态机的 ListDue/processState 循环，
//     harvest 只是 probe_mode 的一个值——不另起 ticker、不另起 job 池。
//
// 状态机（probe_mode='harvest'）：
//   进入：问题号（判死 pending_replace 被打票线接管，或双信号熔断号）。
//   进入动作：原静态桶存进 OriginalProxyID → 迁动态桶（novproxy，每针换IP）
//           → 排资格节奏针（分钟级）。
//   每针（processState 主循环照常跑探针+采票）：
//   - 采到白名单长度票（HarvestOpenAICodexTicket 已入库）→ 回原静态桶
//     → probe_mode='qualification'（复检=在静态出口上 1 针结业上岗，
//     复检必须在将来接业务的静态出口上打——2026-09-21 用户裁定）。
//   - 动态上仍是降级长度（356）→ harvest_attempts++ → 未达上限继续换IP再采
//     （下一针 novproxy 又是新出口）。
//   - harvest_attempts 达上限（openAIDowngradeHarvestMaxAttempts）→ 账号级
//     判定：回原静态桶 + pending_replace（换票无解，转静置线/人工）。
//   - 401/403：凭据问题，采票救不了 → 回原桶 + pending_replace。
//   - 429：走既有 applyRateLimitDeferral（限流不是降智证据，不烧停打闸）。
//
// 采票成功的判据不看 result.TurnStateLen（那是上一针的观测），看票表：
// GetOpenAICodexTicket 返回同账号+模型+harvested_mode=dynamic 的活票即成功
// ——与注入侧三重匹配同一数据源，避免两套口径。
// =============================================================================

const (
	// openAIDowngradeHarvestMaxAttempts 动态桶采票尝试上限。社区经验：好票
	// 通常 1-3 针内出（DouDOU：欧洲出口易出，美出口难）；超过 5 针仍全是
	// 降级长度 = 大概率账号级，硬打只会升级惩罚窗。
	openAIDowngradeHarvestMaxAttempts = 5
	// openAIDowngradeHarvestInterval 采票针间隔：资格节奏同款（jitter 后
	// 2.5-7.5min）——比常规探针密（换IP每针都是新出口，无同出口节流负担），
	// 但 novproxy 网关有 ~20% 连接失败率，密针烂针都不心疼。
	openAIDowngradeHarvestInterval = 5 * time.Minute
)

// errOpenAIHarvestUnavailable 打票线不可用（无动态桶/仓储能力缺失）：
// 409 Conflict——前端可提示「无动态 IP 源，打票线不可用」而非系统错误。
var errOpenAIHarvestUnavailable = infraerrors.Conflict(
	"OPENAI_HARVEST_UNAVAILABLE", "no dynamic harvest bucket available")

// Browser-authorized accounts must use the same route for authorization,
// probes, and production traffic. Dynamic-IP harvesting is therefore limited
// to OpenAI account types without a protected browser authorization route.
var errOpenAIHarvestRouteProtected = infraerrors.Conflict(
	"OPENAI_HARVEST_ROUTE_PROTECTED",
	"browser-authorized OpenAI accounts must keep their authorization proxy; dynamic-IP harvesting is disabled",
)

// 打票线事件常量（openai_downgrade_probe_events.event_type）。
const (
	OpenAIDowngradeEventHarvestStarted        = "harvest_started"
	OpenAIDowngradeEventHarvestTicketAcquired = "harvest_ticket_acquired"
	OpenAIDowngradeEventHarvestAbandoned      = "harvest_abandoned"
)

// OpenAIHarvestBucketFinder 窄可选能力：找动态桶（novproxy）。真实 repo
// 实现；测试桩不实现则打票线静默不进（FailClosed：没有动态源就别打）。
type OpenAIHarvestBucketFinder interface {
	FindOpenAIDynamicHarvestBucket(ctx context.Context) (*int64, error)
}

// StartHarvestOpenAIAccount 手动入口（问题号标签点击→转打票线，相位B）：
// 判死号（pending_replace）或熔断号的救援动作。audit 由 handler 层落。
// 返回下一次采票时间（前端回显「打票中」）。
func (r *OpenAIDowngradeProbeRunner) StartHarvestOpenAIAccount(
	ctx context.Context,
	accountID int64,
) (*OpenAIDowngradeProbeState, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("openai probe runner is not available")
	}
	if r.IsStopped() {
		return nil, errors.New("openai probe runner is stopped")
	}
	now := r.now()
	state, err := r.store.GetOpenAIDowngradeState(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, ErrAccountNotFound
	}
	account, err := r.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if !isOpenAIDowngradeProbeAccountEligible(account, now) {
		return nil, errOpenAIProbeNotEligible
	}
	if IsOpenAIBrowserOAuthAccount(account) {
		return nil, errOpenAIHarvestRouteProtected
	}
	// manual_paused 刹车不替用户松（与 reenable 同纪律）：静置中的号打针
	// 违反社区救援剧本。
	if controls, ok := r.store.(OpenAIDowngradeProbeControlStore); ok {
		if allowed, err := controls.CanRunOpenAIDowngradeProbe(ctx, accountID); err == nil && !allowed {
			return nil, errOpenAIReenablePaused
		}
	}
	if state.State != OpenAIDowngradeStatePendingReplace &&
		state.State != OpenAIDowngradeStateCircuitOpen &&
		state.ProbeMode != "harvest" {
		// 非问题号不需要打票（正常号自己有票——探针顺带采）。
		return nil, errOpenAIHarvestNotProblem
	}
	if err := r.beginHarvest(ctx, state, now); err != nil {
		return nil, err
	}
	return state, nil
}

// errOpenAIHarvestNotProblem 非问题号点打票：400——正常号探针本来就顺带
// 采票，不需要专门进打票线。
var errOpenAIHarvestNotProblem = infraerrors.BadRequest(
	"OPENAI_HARVEST_NOT_PROBLEM", "account is not in a problem state; harvesting is for degraded accounts")

// maybeAutoHarvestDead 自动救援钩子（2026-09-22 用户裁定「都让自动，救成功
// 就立即使用」，推翻 r17x 判死即终态的手动门槛）：RunOnce 扫描循环每分钟
// 对判死号检查——静默期满（NextProbeAt 到期，abandonHarvest/finishReplacement
// 排的 ~24h 冷却）即自动进打票线，走与手动按钮完全相同的
// StartHarvestOpenAIAccount 路径（仓库侧豁免形状②已放行
// pending_replace→动态桶）。救成后既有链路自动上岗（采票→回原桶→复检
// 1 针结业→SetSchedulable=true）。
//
// 边界（与方案对齐）：
//   - manual_paused 不替用户松（StartHarvest 内部同款闸）：静置剧本优先。
//   - status != active 不自动（凭据死号 401 采票救不了，重授权后自然接手）。
//   - 静默未满不动：NextProbeAt 在将来 = 社区反滥用剧本的冷却窗仍在生效，
//     反复测试会使惩罚窗阶梯升级，不能提前打。
//   - 进线失败（无动态桶/瞬时 DB）让位 +30min（spread）：防止每分钟空转
//     重试形成可聚类节律（1136 冻结空打教训）。
func (r *OpenAIDowngradeProbeRunner) maybeAutoHarvestDead(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if state == nil || state.State != OpenAIDowngradeStatePendingReplace ||
		state.ProbeMode == "harvest" {
		return nil
	}
	if now.Before(state.NextProbeAt) {
		return nil
	}
	if _, err := r.StartHarvestOpenAIAccount(ctx, state.AccountID); err != nil {
		if errors.Is(err, errOpenAIHarvestRouteProtected) {
			// Protected browser-authorized accounts remain terminal on their
			// original route. Do not retry the denied dynamic-route transition
			// every minute.
			return nil
		}
		// 让位重排：真 store 读回+并发赢家保护由 yieldFailedCommit 语义
		// 覆盖不了这里（StartHarvest 自己管状态），此处只把静默窗推远
		// 一拍，下轮扫描再看。错误上抛会打断整个扫描循环（一个号卡死
		// 全池），降级为日志由调用方落。
		state.NextProbeAt = now.Add(r.spread(openAIDowngradeHalfOpenInterval))
		state.UpdatedAt = now
		return r.store.SaveOpenAIDowngradeState(ctx, state)
	}
	return nil
}

// beginHarvest 问题号进打票线：迁动态桶 + probe_mode='harvest' + 分钟级
// 采票节奏。调用点=打票线入口（问题号标签点击/判死号被实验接管）。
// 幂等：已在 harvest 模式不重复迁桶。
func (r *OpenAIDowngradeProbeRunner) beginHarvest(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
) error {
	if state.ProbeMode == "harvest" {
		return nil
	}
	finder, ok := r.store.(OpenAIHarvestBucketFinder)
	if !ok {
		return errOpenAIHarvestUnavailable
	}
	dynamicID, err := finder.FindOpenAIDynamicHarvestBucket(ctx)
	if err != nil {
		return err
	}
	if dynamicID == nil {
		return errOpenAIHarvestUnavailable
	}
	// 原静态桶留在 OriginalProxyID（复检回桶用；已是动态桶则保持）。
	if state.OriginalProxyID == nil || *state.OriginalProxyID == *dynamicID {
		state.OriginalProxyID = state.CurrentProxyID
	}
	if err := r.store.SetOpenAIAccountProxy(ctx, state.AccountID, dynamicID); err != nil {
		return err
	}
	state.CurrentProxyID = dynamicID
	state.ProbeMode = "harvest"
	state.State = OpenAIDowngradeStateOnDuty
	state.HarvestAttempts = 0
	state.ConsecutiveFailures = 0
	state.ConsecutiveSuccesses = 0
	state.CircuitOpenedAt = nil
	state.RecoveryDeadline = nil
	state.FirstFailureAt = nil
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeHarvestInterval))
	state.UpdatedAt = now
	if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
		return err
	}
	return r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, dynamicID,
		OpenAIDowngradeEventHarvestStarted, map[string]any{
			"from_proxy_id": state.OriginalProxyID,
			"to_proxy_id":   dynamicID,
		})
}

// processHarvest harvest 模式每针处理（processState 在 runProbe+record 之后
// 调用）。返回值=已接管（调用方直接 return，不再走 Apply 常规迁移）。
func (r *OpenAIDowngradeProbeRunner) processHarvest(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	result *OpenAIDowngradeProbeResult,
	now time.Time,
) (bool, error) {
	// 采到票了吗：查票表动态票（与注入侧同数据源）。harvest 模式的探针
	// 出口=动态桶，HarvestOpenAICodexTicket 以 harvested_mode=dynamic 入库。
	ticketStore, ok := r.store.(OpenAICodexTicketStore)
	if ok {
		if ticket := GetInjectableOpenAICodexTicket(
			ctx, ticketStore, state.AccountID, "gpt-6-astra", state.CurrentProxyID, now,
		); ticket != nil && ticket.HarvestedMode == OpenAICodexTicketHarvestDynamic {
			// 采到健康票：回原静态桶复检（qualification 1 针结业——复检必须
			// 在将来接业务的静态出口上打）。
			homeID := state.OriginalProxyID
			if homeID == nil {
				if mainID, findErr := r.store.FindOpenAIDowngradeMainProxy(ctx, state.AccountID); findErr == nil {
					homeID = mainID
				}
			}
			if homeID == nil {
				homeID = state.CurrentProxyID // 无家可回：动态桶上直接复检
			}
			if err := r.store.SetOpenAIAccountProxy(ctx, state.AccountID, homeID); err != nil {
				return true, err
			}
			state.CurrentProxyID = homeID
			state.ProbeMode = "qualification"
			state.State = OpenAIDowngradeStateOnDuty
			state.ConsecutiveFailures = 1 // r17y 一击退出语义：复检失败即回判死
			state.ConsecutiveSuccesses = 0
			// 复活观察窗（2026-09-22 用户裁定「打票复活的号频率适当高一点」）：
			// 复检通过毕业时读到非空 Deadline → 进 accelerated 盯防 2h；复检
			// 失败回判死时该值随 finishReplacement/qualification_failed 落地
			// 无效化，不污染判死排期。
			state.RecoveryDeadline = timePtr(now.Add(openAIDowngradeHarvestRevivalWatchWindow))
			state.NextProbeAt = now.Add(r.jitter(openAIDowngradeQualificationInterval))
			state.UpdatedAt = now
			if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
				return true, err
			}
			return true, r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, homeID,
				OpenAIDowngradeEventHarvestTicketAcquired, map[string]any{
					"ticket_len":   ticket.TicketLen,
					"issued_at":    ticket.IssuedAt.Format(time.RFC3339),
					"return_proxy": homeID,
					"next":         "qualification_on_static",
				})
		}
	}
	// 没采到票：分诊。
	if result != nil && (result.HTTPStatus == http.StatusUnauthorized || result.HTTPStatus == http.StatusForbidden) {
		// 凭据失效：采票救不了，回原桶判死（社区 isStopStatus 同款）。
		return true, r.abandonHarvest(ctx, state, now, "credentials_invalid")
	}
	state.HarvestAttempts++
	if state.HarvestAttempts >= openAIDowngradeHarvestMaxAttempts {
		// 账号级降智实锤：换票无解（社区：账号级 312 永续=放弃，硬打升级惩罚）。
		return true, r.abandonHarvest(ctx, state, now, "account_level_degraded")
	}
	// 继续换 IP 再采（novproxy 每针新出口）。
	state.NextProbeAt = now.Add(r.jitter(openAIDowngradeHarvestInterval))
	state.LastProbeAt = &now
	state.UpdatedAt = now
	if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
		return true, err
	}
	return true, nil
}

// abandonHarvest 打票线放弃：回原静态桶 + 判死终态。reason 记事件。
func (r *OpenAIDowngradeProbeRunner) abandonHarvest(
	ctx context.Context,
	state *OpenAIDowngradeProbeState,
	now time.Time,
	reason string,
) error {
	homeID := state.OriginalProxyID
	if homeID == nil {
		homeID = state.CurrentProxyID
	}
	if homeID != nil && state.CurrentProxyID != nil && *homeID != *state.CurrentProxyID {
		if err := r.store.SetOpenAIAccountProxy(ctx, state.AccountID, homeID); err != nil {
			return err
		}
	}
	state.CurrentProxyID = homeID
	state.State = OpenAIDowngradeStatePendingReplace
	state.ProbeMode = "normal"
	state.NextProbeAt = now.Add(r.spread(openAIDowngradeReplacementWindow))
	state.UpdatedAt = now
	if err := r.accountRepo.SetSchedulable(ctx, state.AccountID, false); err != nil {
		return err
	}
	if err := r.store.SaveOpenAIDowngradeState(ctx, state); err != nil {
		return err
	}
	return r.store.AppendOpenAIDowngradeEvent(ctx, state.AccountID, homeID,
		OpenAIDowngradeEventHarvestAbandoned, map[string]any{
			"reason":           reason,
			"harvest_attempts": state.HarvestAttempts,
			"return_proxy":     homeID,
		})
}
