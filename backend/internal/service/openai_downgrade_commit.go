package service

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"
)

var (
	ErrOpenAIProbeStale           = errors.New("OpenAI probe generation changed")
	ErrOpenAIProbeAtomicStore     = errors.New("OpenAI probe atomic commit unavailable")
	ErrOpenAIProbeControlStore    = errors.New("OpenAI probe control store unavailable")
	ErrOpenAIProbeSnapshotStore   = errors.New("OpenAI probe account snapshot writer unavailable")
	ErrOpenAIProbeSnapshotRefresh = errors.New("OpenAI probe committed; account snapshot refresh failed")
)

// A probe does network work without locks, then commits only if both database
// generations still match. The mutation contains no credentials.
type OpenAIDowngradeMutation struct {
	AccountID                int64
	ExpectedAccountUpdatedAt time.Time
	ExpectedStateUpdatedAt   time.Time
	ExpectedProxyID          *int64
	ExpectedStatus           string
	ExpectedSchedulable      bool
	State                    *OpenAIDowngradeProbeState
	ProxyChanged             bool
	ProxyID                  *int64
	Schedulable              *bool
	CompleteQualification    bool
	FallbackMode             *bool
	RateLimitResetAt         *time.Time
	RateLimitClear           *OpenAIDowngradeRateLimitObservation
	ErrorMessage             *string
	RecoverOwnedError        bool
	Results                  []OpenAIDowngradeProbeResult
	Events                   []OpenAIDowngradeMutationEvent
}

type OpenAIDowngradeRateLimitObservation struct {
	LimitedAt time.Time
	ResetAt   time.Time
}

type OpenAIDowngradeMutationEvent struct {
	ProxyID    *int64
	Type       string
	Details    json.RawMessage
	ObservedAt *time.Time
}

type OpenAIDowngradeAtomicStore interface {
	CommitOpenAIDowngradeMutation(context.Context, *OpenAIDowngradeMutation) error
}

type OpenAIDowngradeAccountSnapshotStore interface {
	SyncOpenAIDowngradeAccountSnapshot(context.Context, int64) error
}

type OpenAIDowngradeProbeControlStore interface {
	CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error)
}

func (m *OpenAIDowngradeMutation) ChangesAccount() bool {
	return m.ProxyChanged || m.Schedulable != nil || m.CompleteQualification || m.FallbackMode != nil ||
		m.RateLimitResetAt != nil || m.RateLimitClear != nil || m.ErrorMessage != nil || m.RecoverOwnedError
}

type openAIProbeStaging struct {
	OpenAIDowngradeProbeStore
	AccountRepository
	account  *Account
	mutation OpenAIDowngradeMutation
}

func newOpenAIProbeStaging(r *OpenAIDowngradeProbeRunner, account *Account, state *OpenAIDowngradeProbeState) *openAIProbeStaging {
	snapshot := *account
	snapshot.ProxyID = cloneOpenAIProbePointer(account.ProxyID)
	snapshot.Extra = maps.Clone(account.Extra)
	snapshot.Credentials = maps.Clone(account.Credentials)
	return &openAIProbeStaging{
		OpenAIDowngradeProbeStore: r.store,
		AccountRepository:         r.accountRepo,
		account:                   &snapshot,
		mutation: OpenAIDowngradeMutation{
			AccountID:                account.ID,
			ExpectedAccountUpdatedAt: account.UpdatedAt,
			ExpectedStateUpdatedAt:   state.UpdatedAt,
			ExpectedProxyID:          cloneOpenAIProbePointer(account.ProxyID),
			ExpectedStatus:           account.Status,
			ExpectedSchedulable:      account.Schedulable,
		},
	}
}

func cloneOpenAIProbePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s *openAIProbeStaging) checkAccount(id int64) error {
	if id != s.mutation.AccountID {
		return ErrOpenAIProbeStale
	}
	return nil
}

func (s *openAIProbeStaging) GetByID(_ context.Context, id int64) (*Account, error) {
	if err := s.checkAccount(id); err != nil {
		return nil, err
	}
	return s.account, nil
}

func (s *openAIProbeStaging) SaveOpenAIDowngradeState(_ context.Context, state *OpenAIDowngradeProbeState) error {
	if state == nil {
		return errors.New("OpenAI probe state is required")
	}
	if err := s.checkAccount(state.AccountID); err != nil {
		return err
	}
	copy := *state
	s.mutation.State = &copy
	return nil
}

func (s *openAIProbeStaging) SetSchedulable(_ context.Context, id int64, value bool) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	if value && IsOpenAIBrowserOAuthAccount(s.account) {
		qualifiedProxyID, qualified := OpenAIOAuthQualifiedProxyID(s.account.Extra)
		if qualified {
			if s.account.ProxyID == nil || qualifiedProxyID != *s.account.ProxyID {
				// 换票主线（r17u）：合格戳与现桶不一致时，若本 mutation 的
				// 结果链证明探针已在现桶打出完整健康针（qualification-pass
				// 级：传输OK+200+答对+rt 达标），视为「运营迁桶后现桶复检
				// 合格」——放行并把合格戳随迁到现桶（复用 CompleteQualification
				// 落账路径），而非 409 卡死恢复（生产实证：2026-09-21 动态
				// 桶 332 恢复针被 409 回滚，332 白打）。无健康证据仍拒。
				requalified := false
				for _, result := range s.mutation.Results {
					if result.IsQualificationPass() &&
						sameOpenAIProbeProxy(result.ProxyID, s.account.ProxyID) {
						requalified = true
						break
					}
				}
				if !requalified {
					return ErrOpenAIOAuthProxyMismatch
				}
				s.mutation.CompleteQualification = true
			}
		} else {
			if _, exists := s.account.Extra[OpenAIOAuthQualifiedProxyExtraKey]; exists {
				return ErrOpenAIOAuthProxyBindingCorrupt
			}
			if s.account.ProxyID == nil || *s.account.ProxyID <= 0 {
				return ErrOpenAIOAuthProxyRequired
			}
		}
		if !qualified || isOpenAIDowngradeQualificationCandidate(s.account) {
			qualificationPassed := false
			for _, result := range s.mutation.Results {
				// 结果的代理须匹配「实际探测出口」:老号=snapshot 代理,
				// 新号同轮分桶=mutation 目标代理(mutation.ProxyID)。
				if result.IsQualificationPass() && (sameOpenAIProbeProxy(result.ProxyID, s.account.ProxyID) ||
					(s.mutation.ProxyChanged && sameOpenAIProbeProxy(result.ProxyID, s.mutation.ProxyID))) {
					qualificationPassed = true
					break
				}
			}
			if !qualificationPassed {
				return ErrOpenAIOAuthQualificationRequired
			}
			s.mutation.CompleteQualification = true
		}
	}
	s.mutation.Schedulable = &value
	if value && s.account.Status == StatusError {
		s.mutation.RecoverOwnedError = true
	}
	return nil
}

func (s *openAIProbeStaging) SetOpenAIDowngradeFallbackMode(_ context.Context, id int64, value bool) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	s.mutation.FallbackMode = &value
	return nil
}

func (s *openAIProbeStaging) SetRateLimitedIfLater(_ context.Context, id int64, reset time.Time) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	if s.mutation.RateLimitResetAt == nil || reset.After(*s.mutation.RateLimitResetAt) {
		s.mutation.RateLimitResetAt = &reset
	}
	return nil
}

func (s *openAIProbeStaging) ClearOpenAIRateLimitIfObserved(_ context.Context, id int64, limitedAt, resetAt time.Time) (bool, error) {
	if err := s.checkAccount(id); err != nil {
		return false, err
	}
	if s.account == nil || s.account.Platform != PlatformOpenAI ||
		s.account.RateLimitedAt == nil || s.account.RateLimitResetAt == nil ||
		!s.account.RateLimitedAt.Equal(limitedAt) || !s.account.RateLimitResetAt.Equal(resetAt) {
		return false, nil
	}
	// Stage the observation, not a live write. The commit rechecks both
	// timestamps before publishing the recovery event and scheduler outbox.
	s.mutation.RateLimitClear = &OpenAIDowngradeRateLimitObservation{
		LimitedAt: limitedAt, ResetAt: resetAt,
	}
	return true, nil
}

func (s *openAIProbeStaging) SetError(_ context.Context, id int64, message string) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	s.mutation.ErrorMessage = &message
	paused := false
	s.mutation.Schedulable = &paused
	return nil
}

func (s *openAIProbeStaging) SetOpenAIAccountProxy(_ context.Context, id int64, proxyID *int64) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	if IsOpenAIBrowserOAuthAccount(s.account) && !sameOpenAIProbeProxy(s.account.ProxyID, proxyID) {
		// 从未绑定过代理、也没有合格戳的新号允许首次分桶（r17b 自动
		// 分桶工作流）；一旦绑定过或合格过，代理即受保护不可再改。
		_, qualified := OpenAIOAuthQualifiedProxyID(s.account.Extra)
		if s.account.ProxyID != nil || qualified ||
			s.account.Extra[OpenAIOAuthQualifiedProxyExtraKey] != nil {
			return ErrOpenAIOAuthProxyBindingProtected
		}
	}
	s.mutation.ProxyChanged = true
	s.mutation.ProxyID = cloneOpenAIProbePointer(proxyID)
	return nil
}

func (s *openAIProbeStaging) RecordOpenAIDowngradeProbe(_ context.Context, result *OpenAIDowngradeProbeResult) error {
	if result == nil {
		return errors.New("OpenAI probe result is required")
	}
	if err := s.checkAccount(result.AccountID); err != nil {
		return err
	}
	s.mutation.Results = append(s.mutation.Results, *result)
	return nil
}

func (s *openAIProbeStaging) AppendOpenAIDowngradeEvent(_ context.Context, id int64, proxyID *int64, eventType string, details map[string]any) error {
	if err := s.checkAccount(id); err != nil {
		return err
	}
	payload, err := json.Marshal(details)
	if err != nil {
		return err
	}
	s.mutation.Events = append(s.mutation.Events, OpenAIDowngradeMutationEvent{
		ProxyID: cloneOpenAIProbePointer(proxyID), Type: eventType, Details: payload,
	})
	return nil
}

func (s *openAIProbeStaging) CountOpenAIDowngradeEvents(ctx context.Context, id int64, eventType string, since time.Time) (int, error) {
	if err := s.checkAccount(id); err != nil {
		return 0, err
	}
	counter, ok := s.OpenAIDowngradeProbeStore.(OpenAIDowngradeReplaceEventCounter)
	if !ok {
		return 0, errors.New("OpenAI probe event counter unavailable")
	}
	count, err := counter.CountOpenAIDowngradeEvents(ctx, id, eventType, since)
	if err != nil {
		return 0, err
	}
	for _, event := range s.mutation.Events {
		if event.Type == eventType {
			count++
		}
	}
	return count, nil
}

// 票接口直通（r17u）：stagedRunner 把 runner.store 换成 staging 后，probe()
// 的 `r.store.(OpenAICodexTicketStore)` 断言在 staging 上失败 → 采票静默
// 跳过（生产实证：2026-09-21 动态桶 332 针多根，票表恒 0 行零日志）。
// 票是顺带观察哨，不参与 staging 事务——直通底层真 store，主判定回滚
// 不拖累票（采到的 332 票不因探针 commit 409 而丢）。
func (s *openAIProbeStaging) UpsertOpenAICodexTicket(ctx context.Context, ticket *OpenAICodexTicket) error {
	ts, ok := s.OpenAIDowngradeProbeStore.(OpenAICodexTicketStore)
	if !ok {
		return nil
	}
	return ts.UpsertOpenAICodexTicket(ctx, ticket)
}

func (s *openAIProbeStaging) GetOpenAICodexTicket(ctx context.Context, accountID int64, model string) (*OpenAICodexTicket, error) {
	ts, ok := s.OpenAIDowngradeProbeStore.(OpenAICodexTicketStore)
	if !ok {
		return nil, nil
	}
	return ts.GetOpenAICodexTicket(ctx, accountID, model)
}

func (s *openAIProbeStaging) DeleteExpiredOpenAICodexTickets(ctx context.Context, now time.Time) (int64, error) {
	ts, ok := s.OpenAIDowngradeProbeStore.(OpenAICodexTicketStore)
	if !ok {
		return 0, nil
	}
	return ts.DeleteExpiredOpenAICodexTickets(ctx, now)
}

func (r *OpenAIDowngradeProbeRunner) stagedRunner(stage *openAIProbeStaging) *OpenAIDowngradeProbeRunner {
	// Do not copy the live runner's mutexes, sync.Once values or lifecycle.
	return &OpenAIDowngradeProbeRunner{
		store: stage, accountRepo: stage, proxyRepo: r.proxyRepo,
		tokenProvider: r.tokenProvider, httpUpstream: r.httpUpstream,
		tlsProfiles: r.tlsProfiles, interval: r.interval, now: r.now,
		nextDelay: r.nextDelay, probeFn: r.probeFn, recentTraffic: r.recentTraffic,
		deferCounts: maps.Clone(r.deferCounts),
	}
}

func (r *OpenAIDowngradeProbeRunner) processStateAtomic(ctx context.Context, state *OpenAIDowngradeProbeState, now time.Time) error {
	if state == nil {
		return nil
	}
	committer, ok := r.store.(OpenAIDowngradeAtomicStore)
	if !ok {
		return ErrOpenAIProbeAtomicStore
	}
	allowed, err := r.canRunOpenAIProbe(ctx, state.AccountID)
	if err != nil || !allowed {
		return err
	}
	account, err := r.accountRepo.GetByID(ctx, state.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return ErrOpenAIProbeStale
	}
	stage := newOpenAIProbeStaging(r, account, state)
	candidate := *state
	// A probe-owned authentication error can occur before a circuit opens.
	// Requalify it on its current route instead of stranding an on_duty row.
	if account.Status == StatusError && candidate.State == OpenAIDowngradeStateOnDuty {
		candidate.ConsecutiveSuccesses, candidate.ConsecutiveFailures = 0, 0
		if candidate.ProbeMode == "sol_fallback" {
			candidate.State = OpenAIDowngradeStateCircuitOpen
		} else {
			candidate.ProbeMode = "qualification"
		}
	}
	runner := r.stagedRunner(stage)
	if err := runner.processState(ctx, &candidate, now); err != nil {
		return err
	}
	if runner.abuseSignal != nil {
		for i := len(stage.mutation.Events) - 1; i >= 0; i-- {
			if stage.mutation.Events[i].Type == OpenAIDowngradeEventRealTrafficModelMismatch {
				observedAt := runner.abuseSignal.ObservedAt
				stage.mutation.Events[i].ObservedAt = &observedAt
				break
			}
		}
	}
	if stage.mutation.State == nil {
		return nil
	}
	committed, err := r.commitOpenAIProbeMutation(ctx, committer, &stage.mutation)
	if errors.Is(err, ErrOpenAIProbeStale) {
		// 探针网络往返(拥塞期 p50 可达数十秒)期间,后台的中立键写入
		// (codex 用量倒计时等)会推走账号行代际。失配后用新鲜代际重提一次
		// 已完成的探测结果,不再重探;实质前提变化则作废本轮,下轮重探。
		recommitted, retryErr, attempted := r.retryStaleCommit(ctx, committer, &stage.mutation)
		if attempted {
			committed, err = recommitted, retryErr
		}
	}
	if committed {
		*state = *stage.mutation.State
		r.deferCounts = runner.deferCounts
		if runner.abuseSignal != nil {
			openAIAbuseRouteSignals.AcknowledgeRealTrafficSignal(*runner.abuseSignal)
		}
	}
	return err
}

func (r *OpenAIDowngradeProbeRunner) armAbuseSignalAtomic(ctx context.Context, account *Account, state *OpenAIDowngradeProbeState, now time.Time) error {
	if state == nil || account == nil || state.State != OpenAIDowngradeStateOnDuty ||
		state.ProbeMode != "normal" ||
		state.Consecutive429s > 0 || (account.RateLimitResetAt != nil && account.RateLimitResetAt.After(now)) ||
		!sameOpenAIProbeProxy(state.CurrentProxyID, account.ProxyID) {
		return nil
	}
	signal, ok := openAIAbuseRouteSignals.PeekRealTrafficSignal(account.ID, now)
	if !ok {
		return nil
	}
	committer, ok := r.store.(OpenAIDowngradeAtomicStore)
	if !ok {
		return ErrOpenAIProbeAtomicStore
	}
	allowed, err := r.canRunOpenAIProbe(ctx, account.ID)
	if err != nil || !allowed {
		return err
	}
	stage := newOpenAIProbeStaging(r, account, state)
	candidate := *state
	candidate.NextProbeAt, candidate.UpdatedAt = now, now
	stage.mutation.State = &candidate
	if err := stage.AppendOpenAIDowngradeEvent(ctx, state.AccountID, state.CurrentProxyID,
		OpenAIDowngradeEventRealTrafficModelMismatch, map[string]any{
			"requested_model": signal.RequestedModel,
			"response_model":  signal.ResponseModel,
			"observed_at":     signal.ObservedAt.Format(time.RFC3339),
		}); err != nil {
		return err
	}
	stage.mutation.Events[len(stage.mutation.Events)-1].ObservedAt = &signal.ObservedAt
	// Persist the observation before ListDue applies bucket spacing, admission
	// and the batch limit. A restart can then recover the due recheck without
	// depending on this process-local signal.
	committed, err := r.commitOpenAIProbeMutation(ctx, committer, &stage.mutation)
	if committed {
		*state = *stage.mutation.State
		openAIAbuseRouteSignals.AcknowledgeRealTrafficSignal(signal)
	}
	return err
}

// retryStaleCommit 在代际失配(ErrOpenAIProbeStale)后复用已完成的探测结果
// 重提一次。仅当并发变化是"非实质"时才重试:状态行必须原封未动(否则竞争者
// 已推进计数器,本轮回让位),账号行的 proxy/status/schedulable 必须与探针
// 运行前提一致,且未被手动暂停。返回 (是否提交成功, 错误, 是否进行了重试)。
func (r *OpenAIDowngradeProbeRunner) retryStaleCommit(
	ctx context.Context, committer OpenAIDowngradeAtomicStore, mutation *OpenAIDowngradeMutation,
) (bool, error, bool) {
	freshAccount, err := r.accountRepo.GetByID(ctx, mutation.AccountID)
	if err != nil || freshAccount == nil {
		return false, err, false
	}
	freshState, err := r.store.GetOpenAIDowngradeState(ctx, mutation.AccountID)
	if err != nil || freshState == nil {
		return false, err, false
	}
	if !freshState.UpdatedAt.Equal(mutation.ExpectedStateUpdatedAt) {
		return false, nil, false
	}
	if freshAccount.Status != mutation.ExpectedStatus ||
		freshAccount.Schedulable != mutation.ExpectedSchedulable ||
		!sameOpenAIProbeProxy(freshAccount.ProxyID, mutation.ExpectedProxyID) {
		return false, nil, false
	}
	if allowed, aErr := r.canRunOpenAIProbe(ctx, mutation.AccountID); aErr != nil || !allowed {
		return false, aErr, false
	}
	mutation.ExpectedAccountUpdatedAt = freshAccount.UpdatedAt
	mutation.ExpectedStateUpdatedAt = freshState.UpdatedAt
	mutation.ExpectedProxyID = cloneOpenAIProbePointer(freshAccount.ProxyID)
	mutation.ExpectedStatus = freshAccount.Status
	mutation.ExpectedSchedulable = freshAccount.Schedulable
	committed, cErr := r.commitOpenAIProbeMutation(ctx, committer, mutation)
	return committed, cErr, true
}

func (r *OpenAIDowngradeProbeRunner) armQualificationAtomic(ctx context.Context, account *Account, state *OpenAIDowngradeProbeState, now time.Time) error {
	committer, ok := r.store.(OpenAIDowngradeAtomicStore)
	if !ok {
		return ErrOpenAIProbeAtomicStore
	}
	allowed, err := r.canRunOpenAIProbe(ctx, account.ID)
	if err != nil || !allowed {
		return err
	}
	stage := newOpenAIProbeStaging(r, account, state)
	candidate := *state
	candidate.ProbeMode, candidate.NextProbeAt, candidate.UpdatedAt = "qualification", now, now
	paused := false
	stage.mutation.Schedulable = &paused
	stage.mutation.State = &candidate
	committed, err := r.commitOpenAIProbeMutation(ctx, committer, &stage.mutation)
	if committed {
		*state = *stage.mutation.State
	}
	return err
}

func (r *OpenAIDowngradeProbeRunner) canRunOpenAIProbe(ctx context.Context, accountID int64) (bool, error) {
	controls, ok := r.store.(OpenAIDowngradeProbeControlStore)
	if !ok {
		return false, ErrOpenAIProbeControlStore
	}
	return controls.CanRunOpenAIDowngradeProbe(ctx, accountID)
}

// A cache failure after commit is not a database rollback. Preserve the new
// generation and leave the transactional outbox to retry propagation.
func (r *OpenAIDowngradeProbeRunner) commitOpenAIProbeMutation(
	ctx context.Context, committer OpenAIDowngradeAtomicStore, mutation *OpenAIDowngradeMutation,
) (bool, error) {
	var snapshots OpenAIDowngradeAccountSnapshotStore
	if mutation.ChangesAccount() {
		var ok bool
		snapshots, ok = r.accountRepo.(OpenAIDowngradeAccountSnapshotStore)
		if !ok {
			return false, ErrOpenAIProbeSnapshotStore
		}
	}
	if err := committer.CommitOpenAIDowngradeMutation(ctx, mutation); err != nil {
		return false, err
	}
	if snapshots != nil {
		propagationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := snapshots.SyncOpenAIDowngradeAccountSnapshot(propagationCtx, mutation.AccountID); err != nil {
			return true, ErrOpenAIProbeSnapshotRefresh
		}
	}
	return true, nil
}
