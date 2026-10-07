package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type downgradeProbeAccountRepoStub struct {
	AccountRepository
	listByPlatformFn  func(context.Context, string) ([]Account, error)
	getByIDFn         func(context.Context, int64) (*Account, error)
	account           *Account
	getByIDCalls      int
	getByIDErr        error
	schedulableErr    error
	schedulableCalls  []bool
	errorMessages     []string
	proxyChanges      []*int64
	fallbackModes     []bool
	rateLimitedResets []time.Time
	rateLimitedErr    error
	snapshotErr       error
	snapshotCalls     int
	// 稀疏复查的持有释放（r17）：记录 CAS 清除调用与观察到的 (limited_at,
	// reset_at) 对，供「复查 200 即按 CAS 回岗」回归断言。
	openAIRateLimitClears      []openAIRateLimitClearCall
	openAIRateLimitClearErr    error
	openAIRateLimitClearResult bool
	// 僵尸 state 就地删除（r17）：记录 DeleteOpenAIDowngradeState 调用，
	// 供「账号已软删 → state 当轮删除」回归断言。
	deleteStateCalls []int64
	deleteStateErr   error
}

type openAIRateLimitClearCall struct {
	id        int64
	limitedAt time.Time
	resetAt   time.Time
}

func (s *downgradeProbeAccountRepoStub) SyncOpenAIDowngradeAccountSnapshot(ctx context.Context, _ int64) error {
	s.snapshotCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.snapshotErr
}

func (s *downgradeProbeAccountRepoStub) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	if s.listByPlatformFn != nil {
		return s.listByPlatformFn(ctx, platform)
	}
	if s.account == nil {
		return nil, nil
	}
	return []Account{*s.account}, nil
}

func (s *downgradeProbeAccountRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	s.getByIDCalls++
	if s.getByIDFn != nil {
		return s.getByIDFn(ctx, id)
	}
	if s.getByIDErr != nil {
		return nil, s.getByIDErr
	}
	return s.account, nil
}

func (s *downgradeProbeAccountRepoStub) SetSchedulable(_ context.Context, _ int64, value bool) error {
	s.schedulableCalls = append(s.schedulableCalls, value)
	if s.account != nil {
		s.account.Schedulable = value
	}
	return s.schedulableErr
}

func (s *downgradeProbeAccountRepoStub) SetError(_ context.Context, _ int64, message string) error {
	s.errorMessages = append(s.errorMessages, message)
	return nil
}

func (s *downgradeProbeAccountRepoStub) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	return errors.New("probe must use monotonic rate limit writer")
}

func (s *downgradeProbeAccountRepoStub) SetRateLimitedIfLater(_ context.Context, _ int64, resetAt time.Time) error {
	s.rateLimitedResets = append(s.rateLimitedResets, resetAt)
	return s.rateLimitedErr
}

func (s *downgradeProbeAccountRepoStub) ClearOpenAIRateLimitIfObserved(_ context.Context, id int64, observedLimitedAt, observedResetAt time.Time) (bool, error) {
	s.openAIRateLimitClears = append(s.openAIRateLimitClears, openAIRateLimitClearCall{
		id: id, limitedAt: observedLimitedAt, resetAt: observedResetAt,
	})
	return s.openAIRateLimitClearResult, s.openAIRateLimitClearErr
}

func (s *downgradeProbeAccountRepoStub) SetOpenAIDowngradeFallbackMode(_ context.Context, _ int64, active bool) error {
	s.fallbackModes = append(s.fallbackModes, active)
	if s.account != nil {
		if s.account.Extra == nil {
			s.account.Extra = make(map[string]any)
		}
		s.account.Extra[OpenAIDowngradeSolFallbackExtraKey] = active
	}
	return nil
}

type downgradeProbeStoreStub struct {
	OpenAIDowngradeProbeStore
	ensureFn       func(context.Context, int64, *int64, time.Time) (*OpenAIDowngradeProbeState, error)
	reconcileFn    func(context.Context, time.Time, time.Duration) (int64, error)
	listDueFn      func(context.Context, time.Time, int) ([]OpenAIDowngradeProbeState, error)
	eventCountFn   func(context.Context, int64, string, time.Time) (int, error)
	getStateFn     func(context.Context, int64) (*OpenAIDowngradeProbeState, error)
	state          *OpenAIDowngradeProbeState
	ensureNextAt   time.Time
	ensureCalls    int
	due            []OpenAIDowngradeProbeState
	saveCalls      int
	getStateCalls  int
	probeCalls     int
	probeResults   []OpenAIDowngradeProbeResult
	eventCalls     int
	dashboard      *OpenAIDowngradeDashboard
	events         []OpenAIDowngradeEvent
	accountStats   []OpenAIDowngradeAccountStat
	mainProxyID    *int64
	mainProxyCalls int
	escapeProxyID  *int64
	proxyChanges   []*int64
	eventDetails   []map[string]any
	eventTypes     []string
	saveErr        error
	eventErr       error
	// 僵尸 state 就地删除路径（dropGoneAccountState）的观测字段：
	// deleteStateCalls 记录被删账号ID，deleteStateErr 注入暂态删除故障。
	deleteStateCalls []int64
	deleteStateErr   error
}

func (s *downgradeProbeStoreStub) EnsureOpenAIDowngradeState(
	ctx context.Context, accountID int64, proxyID *int64, nextAt time.Time,
) (*OpenAIDowngradeProbeState, error) {
	s.ensureCalls++
	if s.ensureFn != nil {
		return s.ensureFn(ctx, accountID, proxyID, nextAt)
	}
	s.ensureNextAt = nextAt
	if s.state == nil {
		s.state = &OpenAIDowngradeProbeState{
			AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
			CurrentProxyID: proxyID, OriginalProxyID: proxyID, NextProbeAt: nextAt,
		}
	}
	return s.state, nil
}

func (s *downgradeProbeStoreStub) ListDueOpenAIDowngradeStates(ctx context.Context, now time.Time, limit int) ([]OpenAIDowngradeProbeState, error) {
	if s.listDueFn != nil {
		return s.listDueFn(ctx, now, limit)
	}
	return s.due, nil
}

func (s *downgradeProbeStoreStub) ReconcileOpenAIRateLimitProbeSchedules(ctx context.Context, now time.Time, interval time.Duration) (int64, error) {
	if s.reconcileFn != nil {
		return s.reconcileFn(ctx, now, interval)
	}
	return 0, nil
}

func (s *downgradeProbeStoreStub) DeleteOpenAIDowngradeState(_ context.Context, accountID int64) error {
	s.deleteStateCalls = append(s.deleteStateCalls, accountID)
	return s.deleteStateErr
}

func (s *downgradeProbeStoreStub) SaveOpenAIDowngradeState(_ context.Context, state *OpenAIDowngradeProbeState) error {
	s.saveCalls++
	if s.saveErr != nil {
		return s.saveErr
	}
	if state != nil {
		copy := *state
		s.state = &copy
	}
	return nil
}

func (s *downgradeProbeStoreStub) RecordOpenAIDowngradeProbe(_ context.Context, result *OpenAIDowngradeProbeResult) error {
	s.probeCalls++
	if result != nil {
		s.probeResults = append(s.probeResults, *result)
	}
	return nil
}

// GetOpenAIDowngradeState 供代际失配重试(retryStaleCommit)读取新鲜状态行:
// 未预置 state 时返回 nil,重试按"状态行不可用"让位。
func (s *downgradeProbeStoreStub) GetOpenAIDowngradeState(ctx context.Context, accountID int64) (*OpenAIDowngradeProbeState, error) {
	s.getStateCalls++
	if s.getStateFn != nil {
		return s.getStateFn(ctx, accountID)
	}
	return s.state, nil
}

func (s *downgradeProbeStoreStub) CountOpenAIDowngradeEvents(ctx context.Context, id int64, eventType string, since time.Time) (int, error) {
	if s.eventCountFn != nil {
		return s.eventCountFn(ctx, id, eventType, since)
	}
	return 0, nil
}

func (s *downgradeProbeStoreStub) AppendOpenAIDowngradeEvent(_ context.Context, _ int64, _ *int64, eventType string, details map[string]any) error {
	s.eventCalls++
	s.eventTypes = append(s.eventTypes, eventType)
	s.eventDetails = append(s.eventDetails, details)
	return s.eventErr
}

func (s *downgradeProbeStoreStub) FindOpenAIDowngradeEscapeProxy(context.Context, int64, *int64) (*int64, error) {
	return s.escapeProxyID, nil
}

func (s *downgradeProbeStoreStub) SetOpenAIAccountProxy(_ context.Context, _ int64, proxyID *int64) error {
	s.proxyChanges = append(s.proxyChanges, proxyID)
	return nil
}

func (s *downgradeProbeStoreStub) FindOpenAIDowngradeMainProxy(context.Context, int64) (*int64, error) {
	s.mainProxyCalls++
	return s.mainProxyID, nil
}

func (s *downgradeProbeStoreStub) ListOpenAIDowngradeBuckets(context.Context) ([]OpenAIDowngradeBucket, error) {
	return nil, nil
}

func (s *downgradeProbeStoreStub) ListOpenAIDowngradeDashboard(context.Context, time.Time) (*OpenAIDowngradeDashboard, error) {
	return s.dashboard, nil
}

func (s *downgradeProbeStoreStub) ListOpenAIDowngradeEvents(context.Context, time.Time, int) ([]OpenAIDowngradeEvent, error) {
	return s.events, nil
}

func (s *downgradeProbeStoreStub) ListOpenAIDowngradeAccountStats(context.Context, time.Time, int) ([]OpenAIDowngradeAccountStat, error) {
	return s.accountStats, nil
}

func downgradeProbeIntPtr(v int) *int { return &v }

// 纯单针杀（2026-09-22 用户裁定，1143 弹跳形态实证）：normal 档任何一针
// 降智证据当场熔断；qualification 新号线维持 2 连败（2026-09-21 裁定）。
func TestApplyOpenAIDowngradeProbeResultSingleFailureCircuitsNormalMode(t *testing.T) {
	// normal 档：首针降智即熔断。
	state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"}
	failed := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(516),
	}
	single := ApplyOpenAIDowngradeProbeResult(state, failed, time.Now())
	require.True(t, single.Circuit)
	require.Equal(t, OpenAIDowngradeStateCircuitOpen, single.NextState)
	require.Equal(t, OpenAIDowngradeEventCircuitOpen, single.EventType)

	// qualification 新号线：首针失败不熔断（新号无历史基线，防 IP 级暂态误杀）。
	qual := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification"}
	firstQual := ApplyOpenAIDowngradeProbeResult(qual, failed, time.Now())
	require.False(t, firstQual.Circuit)
	require.Equal(t, OpenAIDowngradeStateOnDuty, firstQual.NextState)

	// qualification 2 连败仍判死（既有节奏不变）。
	qual.ConsecutiveFailures = 1
	secondQual := ApplyOpenAIDowngradeProbeResult(qual, failed, time.Now())
	require.True(t, secondQual.Circuit)

	// reprobe 档单针失败 → 判死（救不回的号快速进打票线）。
	reprobe := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateReprobe, ProbeMode: "normal"}
	dead := ApplyOpenAIDowngradeProbeResult(reprobe, failed, time.Now())
	require.True(t, dead.NeedsReplacement)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, dead.NextState)
	require.Equal(t, OpenAIDowngradeEventReplaceRequired, dead.EventType)
}

func TestOpsServiceGetOpenAIDowngradeDashboardBuildsReadOnlySnapshot(t *testing.T) {
	store := &downgradeProbeStoreStub{
		dashboard: &OpenAIDowngradeDashboard{
			Buckets: []OpenAIDowngradeBucket{
				{ProxyID: 3, Capacity: 1, AccountIDs: []int64{101}},
			},
			ProbeCount24h:   4,
			SuccessCount24h: 2,
		},
		events: []OpenAIDowngradeEvent{{ID: 7, EventType: OpenAIDowngradeEventCircuitOpen}},
		accountStats: []OpenAIDowngradeAccountStat{
			{AccountID: 101, State: OpenAIDowngradeStateCircuitOpen},
			{AccountID: 102, State: OpenAIDowngradeStateOnDuty},
		},
	}
	svc := &OpsService{openAIDowngradeStore: store}

	got, err := svc.GetOpenAIDowngradeDashboard(context.Background(), time.Time{}, 20, 20)
	require.NoError(t, err)
	require.Len(t, got.RecentEvents, 1)
	require.Len(t, got.AccountStats, 2)
	require.Equal(t, int64(1), got.DegradedAccountCount)
	require.Equal(t, int64(1), got.AtCapacityBucketCount)
	require.True(t, got.Buckets[0].AtCapacity)
}

func TestApplyOpenAIDowngradeProbeResultRecoveryRequiresTwoHealthyResults(t *testing.T) {
	state := OpenAIDowngradeProbeState{
		State:               OpenAIDowngradeStateReprobe,
		ConsecutiveFailures: 2,
	}
	healthy := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
	}

	first := ApplyOpenAIDowngradeProbeResult(state, healthy, time.Now())
	require.False(t, first.Recovered)

	state.ConsecutiveSuccesses = 1
	second := ApplyOpenAIDowngradeProbeResult(state, healthy, time.Now())
	require.True(t, second.Recovered)
	require.Equal(t, OpenAIDowngradeEventRecovered, second.EventType)
}

func TestOpenAIDowngradeProbeResultThresholdsAreStrict(t *testing.T) {
	low := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeFailureReasoningThreshold - 1),
	}
	boundary := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeFailureReasoningThreshold),
	}
	recovered := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
	}

	require.False(t, low.IsDegraded())
	require.False(t, low.IsQualificationPass())
	require.False(t, boundary.IsDegraded())
	require.False(t, boundary.IsRecovered())
	require.True(t, recovered.IsRecovered())
}

func TestOpenAIDowngradeProbeTruncationFingerprintsSplitByAnswer(t *testing.T) {
	// 2026-09-15 用户裁定：指纹+答对=中性（预算截断未伤结论，1034/1035 实测
	// rt 恒落 1552 且答案正确）；指纹+答错=降智。正确低 token 同样中性。
	for _, tokens := range []int{508, 516, 524, 1026, 1034, 1042, 1544, 1552, 1560} {
		wrong := OpenAIDowngradeProbeResult{
			TransportOK:     true,
			AnswerCorrect:   false,
			ReasoningTokens: downgradeProbeIntPtr(tokens),
		}
		require.Truef(t, wrong.IsDegraded(), "tokens=%d wrong answer should be degraded", tokens)
		require.Falsef(t, wrong.IsRecovered(), "tokens=%d wrong answer should not recover", tokens)

		correct := OpenAIDowngradeProbeResult{
			TransportOK:     true,
			AnswerCorrect:   true,
			ReasoningTokens: downgradeProbeIntPtr(tokens),
		}
		require.Falsef(t, correct.IsRecovered(), "tokens=%d fingerprint never counts as recovery", tokens)
		require.Falsef(t, correct.IsDegraded(), "tokens=%d correct answer should be neutral", tokens)
	}
}

func TestApplyOpenAIDowngradeFingerprintCorrectIsNeutralAndKeepsStreak(t *testing.T) {
	// 1034/1035 实测画像：rt 恒落 1552 且答案正确。正常档的中性针不清连胜、
	// 不计失败、不熔断（认证档的答对主义连胜见
	// TestApplyOpenAIDowngradeQualificationCountsAnswerCorrectStreak）。
	state := OpenAIDowngradeProbeState{
		State:                OpenAIDowngradeStateOnDuty,
		ProbeMode:            "normal",
		ConsecutiveSuccesses: 1,
	}
	neutral := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(1552),
	}

	tr := ApplyOpenAIDowngradeProbeResult(state, neutral, time.Now())
	require.Equal(t, 1, tr.State.ConsecutiveSuccesses, "neutral keeps success streak")
	require.Equal(t, 0, tr.State.ConsecutiveFailures)
	require.False(t, tr.Circuit)

	// 连败边缘：fails=1 时中性针不熔断（旧规则会计成第 2 败直接停用）。
	state.ConsecutiveFailures = 1
	tr = ApplyOpenAIDowngradeProbeResult(state, neutral, time.Now())
	require.False(t, tr.Circuit)
	require.Equal(t, 0, tr.State.ConsecutiveFailures, "neutral clears failure streak")

	// 干净针跨过中性针凑齐 2 连胜（解锁判定在 runner 侧，此处验证计数到位）。
	clean := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(1597),
	}
	tr = ApplyOpenAIDowngradeProbeResult(state, clean, time.Now())
	require.Equal(t, 2, tr.State.ConsecutiveSuccesses)
}

func TestApplyOpenAIDowngradeQualificationCountsAnswerCorrectStreak(t *testing.T) {
	// 认证档答对主义(2026-09-15 用户裁定「他只要答对就行」): 1552+答对在
	// qualification 模式下进连胜(1035 实测 rt 恒 1552 连续答对,旧规则认证
	// 永远凑不齐);正常模式同一针仍是中性,不进连胜。
	qual := OpenAIDowngradeProbeState{
		State:     OpenAIDowngradeStateOnDuty,
		ProbeMode: "qualification",
	}
	neutral := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(1552),
	}
	require.True(t, neutral.IsQualificationPass())
	require.False(t, neutral.IsRecovered())

	tr := ApplyOpenAIDowngradeProbeResult(qual, neutral, time.Now())
	require.Equal(t, 1, tr.State.ConsecutiveSuccesses, "qualification counts fingerprint+correct as streak")

	second := ApplyOpenAIDowngradeProbeResult(tr.State, neutral, time.Now())
	// 连胜计数本身保留通用语义（熔断恢复等路径仍用 ≥2）；runner 侧 r15h 起
	// 认证 1 针即解锁，第 2 针只在异常序列（解锁失败重试）中出现。
	require.Equal(t, 2, second.State.ConsecutiveSuccesses, "streak counter still accumulates")

	normal := OpenAIDowngradeProbeState{
		State:     OpenAIDowngradeStateOnDuty,
		ProbeMode: "normal",
	}
	tr = ApplyOpenAIDowngradeProbeResult(normal, neutral, time.Now())
	require.Equal(t, 0, tr.State.ConsecutiveSuccesses, "normal mode keeps fingerprint+correct neutral")

	// 答错/低rt 仍不进连胜。
	wrong := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(1552),
	}
	tr = ApplyOpenAIDowngradeProbeResult(qual, wrong, time.Now())
	require.Equal(t, 0, tr.State.ConsecutiveSuccesses)
	require.Equal(t, 1, tr.State.ConsecutiveFailures)

	lowRT := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   true,
		ReasoningTokens: downgradeProbeIntPtr(700),
	}
	require.False(t, lowRT.IsQualificationPass())
}

func TestParseOpenAIDowngradeProbeResponseSupportsJSONAndSSE(t *testing.T) {
	jsonBody := []byte(`{"output":[{"content":[{"text":"答案是21"}]}],"usage":{"reasoning_tokens":1992,"juice":21}}`)
	answerCorrect, reasoningTokens, juice := parseOpenAIDowngradeProbeResponse(jsonBody, openAIDowngradeNumericAnswerPattern(21))
	require.True(t, answerCorrect)
	require.NotNil(t, reasoningTokens)
	require.NotNil(t, juice)
	require.Equal(t, 1992, *reasoningTokens)
	require.Equal(t, 21, *juice)

	sseBody := []byte("event: response.output_text.delta\n" +
		"data: {\"delta\":\"答案是21\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"usage\":{\"reasoning_tokens\":1992,\"juice\":21}}\n\n")
	answerCorrect, reasoningTokens, juice = parseOpenAIDowngradeProbeResponse(sseBody, openAIDowngradeNumericAnswerPattern(21))
	require.True(t, answerCorrect)
	require.NotNil(t, reasoningTokens)
	require.NotNil(t, juice)
	require.Equal(t, 1992, *reasoningTokens)
	require.Equal(t, 21, *juice)
}

// TestParseOpenAIDowngradeProbeResponseAcceptsItemDoneDelivery（2026-09-18
// 服务端流形态变更回归）：终态 response.completed 不再回显 output 条目
// （response.output=[]，1077 冻结流实证），答案全文经 output_item.done 的
// message 条目交付。此前该形态死于「终态 output 走查空文本」，是 1070-1077
// 批次资格探针 200-无-usage 0/N 的真根因（服务端 9/17 05:25 后改形态，
// 最后一次通过=1060 05:25:13）。
func TestParseOpenAIDowngradeProbeResponseAcceptsItemDoneDelivery(t *testing.T) {
	// 生产冻结流骨架：reasoning 条目 + delta 流（含错答文本）+ message 条目
	// （正答）+ 空回显终态。delta 文本与条目文本并存 → 判分只认条目终文。
	sseBody := []byte(
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_probe\"}}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"status\":\"completed\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":1,\"content_index\":0,\"delta\":\"错答 7\"}\n\n" +
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"output\":[],\"usage\":{\"output_tokens\":1960,\"output_tokens_details\":{\"reasoning_tokens\":1552}}}}\n\n" +
			"data: [DONE]\n\n")
	correct, tokens, juice := parseOpenAIDowngradeProbeResponse(sseBody, openAIDowngradeNumericAnswerPattern(21))
	require.True(t, correct)
	require.NotNil(t, tokens)
	require.Equal(t, 1552, *tokens)
	require.Nil(t, juice)

	// 旧形态（终态带回显）优先级不变：条目文本与终态回显冲突时判分认终态。
	legacyEcho := []byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"错答 7\"}]}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}}\n\n")
	correct, tokens, _ = parseOpenAIDowngradeProbeResponse(legacyEcho, openAIDowngradeNumericAnswerPattern(21))
	require.True(t, correct)
	require.NotNil(t, tokens)
	require.Equal(t, 1992, *tokens)

	// 条目文本不是终态证据的替代品：无 response.completed 仍拒收（与既有
	// done_without_completion 语义一致）。
	noTerminal := []byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}}\n\n" +
			"data: [DONE]\n\n")
	correct, tokens, juice = parseOpenAIDowngradeProbeResponse(noTerminal, openAIDowngradeNumericAnswerPattern(21))
	require.False(t, correct)
	require.Nil(t, tokens)
	require.Nil(t, juice)

	// 未完成条目与终态走查同罪：status=incomplete 硬拒，不得经条目通道放行。
	incompleteItem := []byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"incomplete\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"reasoning_tokens\":1992}}}\n\n")
	correct, tokens, juice = parseOpenAIDowngradeProbeResponse(incompleteItem, openAIDowngradeNumericAnswerPattern(21))
	require.False(t, correct)
	require.Nil(t, tokens)
	require.Nil(t, juice)
}

// TestParseOpenAIDowngradeProbeCompletionReturnsAnswerText（r17am 留档配套）：
// 判分无关内核必须返回与判分所用的同一份答案全文——条目终文 / 终态回显 /
// legacy delta 三种交付形态都要拿到文本；一切结构失败路径统一空文本。
// 此前留档只存响应尾 8KB，终态 usage 记录霸占尾部，答案文本几乎总被截掉
// （9/28 团灭复盘实证），答错定性只能靠 rt 侧写。
func TestParseOpenAIDowngradeProbeCompletionReturnsAnswerText(t *testing.T) {
	itemDone := []byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"错答 7\"}]}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"reasoning_tokens\":516}}}\n\n")
	text, tokens, _ := parseOpenAIDowngradeProbeCompletion(itemDone)
	require.Equal(t, "错答 7\n", text)
	require.NotNil(t, tokens)

	legacyEcho := []byte(
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}}\n\n")
	text, tokens, _ = parseOpenAIDowngradeProbeCompletion(legacyEcho)
	require.Equal(t, "答案是21\n", text)
	require.NotNil(t, tokens)

	legacyDelta := []byte("event: response.output_text.delta\n" +
		"data: {\"delta\":\"答案是21\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"usage\":{\"reasoning_tokens\":1992}}\n\n")
	text, tokens, _ = parseOpenAIDowngradeProbeCompletion(legacyDelta)
	require.Equal(t, "答案是21", text)
	require.NotNil(t, tokens)

	// 无终态：空文本 + 零计数（与 wrapper 的 false,nil,nil 同形）。
	noTerminal := []byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"答案是21\"}]}}\n\n")
	text, tokens, _ = parseOpenAIDowngradeProbeCompletion(noTerminal)
	require.Empty(t, text)
	require.Nil(t, tokens)
}

// TestApplyResponseGradedTextAndArchiveAnswerText（r17am）：applyResponse 把
// 判分文本收进 gradedText；留档 JSONL 落 answer_text 字段。nil 判分正则的
// 拒收语义保持（TransportOK=false，gradedText 空）。
func TestApplyResponseGradedTextAndArchiveAnswerText(t *testing.T) {
	sseBody := []byte(
		"data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"错答 7\"}]}}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"reasoning_tokens\":675}}}\n\n")

	result := OpenAIDowngradeProbeResult{HTTPStatus: 200}
	result.applyResponse(sseBody, openAIDowngradeNumericAnswerPattern(21))
	require.True(t, result.TransportOK)
	require.False(t, result.AnswerCorrect)
	require.Equal(t, "错答 7\n", result.gradedText)

	// nil 正则 = 拒收：不产出可判定结果（r17f 教训回归）。
	nilPattern := OpenAIDowngradeProbeResult{HTTPStatus: 200}
	nilPattern.applyResponse(sseBody, nil)
	require.False(t, nilPattern.TransportOK)
	require.Nil(t, nilPattern.ReasoningTokens)
	require.Empty(t, nilPattern.gradedText)

	// 留档全链：answer_text 进 JSONL，超长保尾。
	dir := t.TempDir()
	t.Setenv("SUB2API_PROBE_ARCHIVE_DIR", dir)
	question := openAIDowngradeProbeQuestion{
		Domain: "candy", Text: "糖果题面", AnswerDisplay: "21",
		AnswerPattern: openAIDowngradeNumericAnswerPattern(21),
	}
	archiveOpenAIDowngradeProbe(1195, "normal", question, &result, sseBody)
	archiveOpenAIDowngradeProbe(1194, "normal", question, &result, sseBody)

	raw, err := os.ReadFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.Len(t, lines, 2)
	var entry openAIDowngradeProbeArchiveEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &entry))
	require.Equal(t, int64(1194), entry.AccountID)
	require.Equal(t, "错答 7\n", entry.AnswerText)
	require.Nil(t, entry.AnswerCorrect)
	require.Contains(t, entry.ResponseTail, "response.completed")

	// 超长答案保尾截断（结论在末段）。
	longAnswer := strings.Repeat("前段废话", 1024) + "最终答案 7"
	longResult := OpenAIDowngradeProbeResult{HTTPStatus: 200, TransportOK: true}
	longResult.gradedText = longAnswer
	archiveOpenAIDowngradeProbe(1193, "normal", question, &longResult, nil)
	raw, err = os.ReadFile(filepath.Join(dir, time.Now().Format("2006-01-02")+".jsonl"))
	require.NoError(t, err)
	lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &entry))
	require.Equal(t, int64(1193), entry.AccountID)
	require.LessOrEqual(t, len(entry.AnswerText), openAIDowngradeArchiveAnswerCap)
	require.Contains(t, entry.AnswerText, "最终答案 7")
}

func TestOpenAIProbeTurnStateSignal(t *testing.T) {
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("X-Codex-Primary", "5h:reset")
	length, is292 := openAIProbeTurnStateSignal(200, header)
	require.Zero(t, length)
	require.False(t, is292)

	for _, name := range []string{"current_turn_state", "Current_Turn_State", "Current-Turn-State", "CURRENT_TURN_STATE"} {
		carrier := http.Header{}
		carrier.Set(name, "sig.definitely.not.a.real.credential")
		length, is292 = openAIProbeTurnStateSignal(200, carrier)
		require.Equal(t, len("sig.definitely.not.a.real.credential"), length, name)
		require.False(t, is292, name)
	}
	length, is292 = openAIProbeTurnStateSignal(292, http.Header{})
	require.Zero(t, length)
	require.True(t, is292)
	for _, status := range []int{201, 204, 404, 429, 502} {
		_, is292 = openAIProbeTurnStateSignal(status, nil)
		require.False(t, is292, status)
	}
	_, _ = openAIProbeTurnStateSignal(200, nil)
}

func TestOpenAIProbeCodexTurnStateLen(t *testing.T) {
	carrier := http.Header{}
	carrier.Set("X-Codex-Turn-State", "g"+strings.Repeat("A", 331))
	require.Equal(t, 332, openAIProbeCodexTurnStateLen(carrier))

	for _, name := range []string{"x-codex-turn-state", "X-CODEX-TURN-STATE", "x_codex_turn_state", "X_Codex_Turn_State"} {
		variant := http.Header{}
		variant.Set(name, "token")
		require.Equal(t, 5, openAIProbeCodexTurnStateLen(variant), name)
	}

	absent := http.Header{}
	absent.Set("Content-Type", "text/event-stream")
	absent.Set("x-codex-primary-used-percent", "3")
	require.Zero(t, openAIProbeCodexTurnStateLen(absent))
	require.Zero(t, openAIProbeCodexTurnStateLen(nil))
	require.Zero(t, openAIProbeCodexTurnStateLen(http.Header{"X-Codex-Turn-State": nil}))

	neighbor := http.Header{}
	neighbor.Set("X-Codex-Turn-Metadata", "meta")
	require.Zero(t, openAIProbeCodexTurnStateLen(neighbor))
}

func TestOpenAIProbeParseFailureFieldsAreMetadataOnly(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n", "\r"} {
		t.Run(fmtNewlineName(newline), func(t *testing.T) {
			body := []byte(strings.Join([]string{
				`data: {"type":"response.output_text.delta","delta":"upstream-private-marker"}`,
				"",
				`data: {"type":"response.completed","usage":{"reasoning_tokens":1600}}`,
				"",
			}, newline))
			var output bytes.Buffer
			slog.New(slog.NewJSONHandler(&output, nil)).
				Warn("openai_probe_parse_failed_forensics", openAIProbeParseFailureFields(body)...)
			require.NotContains(t, output.String(), "upstream-private-marker")
			var fields map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &fields))
			require.Equal(t, float64(len(body)), fields["bytes"])
			require.Equal(t, float64(2), fields["data_records"])
			require.Equal(t, true, fields["has_terminal"])
			require.Equal(t, true, fields["has_usage"])
			require.Equal(t, true, fields["has_reasoning"])
			for key := range fields {
				require.Contains(t, []string{"time", "level", "msg", "bytes", "data_records", "has_terminal", "has_usage", "has_reasoning"}, key)
			}
		})
	}
}

func fmtNewlineName(value string) string {
	switch value {
	case "\r\n":
		return "CRLF"
	case "\r":
		return "CR"
	default:
		return "LF"
	}
}

func TestProbeResponseItemDoneDoesNotResurrectDeltas(t *testing.T) {
	for _, tt := range []struct {
		name string
		item string
	}{
		{"refusal", `{"type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"No answer"}]}`},
		{"empty_message", `{"type":"message","role":"assistant","status":"completed","content":[]}`},
		{"blank_text", `{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":" \t\n"}]}`},
		{"reasoning_only", `{"type":"reasoning","status":"completed"}`},
		{"non_assistant", `{"type":"message","role":"user","content":[{"type":"output_text","text":"21"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, finalOutput := range []string{"", `,"output":[]`} {
				body := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\n" +
					"data: {\"type\":\"response.output_item.done\",\"item\":" + tt.item + "}\n\n" +
					"data: {\"type\":\"response.completed\",\"status\":\"completed\",\"usage\":{\"reasoning_tokens\":1992}" + finalOutput + "}\n\n")
				result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
				result.applyResponse(body, openAIDowngradeNumericAnswerPattern(21))
				require.False(t, result.TransportOK)
				require.Nil(t, result.ReasoningTokens)
				require.False(t, result.IsQualificationPass())
				require.False(t, result.IsRecovered())
				require.False(t, result.IsDegraded())
				require.NotEmpty(t, result.ErrorMessage)
			}
		})
	}
}

func TestProbeResponseBlankTextIsInconclusive(t *testing.T) {
	for _, text := range []string{"", " \t\r\n", "\u2003"} {
		encoded, err := json.Marshal(text)
		require.NoError(t, err)
		for _, body := range []string{
			`{"status":"completed","usage":{"reasoning_tokens":1992},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + string(encoded) + `}]}]}`,
			"data: {\"type\":\"response.output_text.delta\",\"delta\":" + string(encoded) + "}\n\n" +
				"data: {\"type\":\"response.completed\",\"usage\":{\"reasoning_tokens\":1992}}\n\n",
		} {
			result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
			result.applyResponse([]byte(body), openAIDowngradeNumericAnswerPattern(21))
			require.False(t, result.TransportOK)
			require.Nil(t, result.ReasoningTokens)
			require.False(t, result.IsDegraded())
			require.False(t, result.IsQualificationPass())
		}
	}
}

func TestProbeResponseStreamFallbackRequiresEmptyTerminalOutput(t *testing.T) {
	for _, delivery := range []struct {
		name   string
		prefix string
		nested bool
	}{
		{
			name:   "item_done",
			prefix: `data: {"type":"response.output_item.done","item":{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"21"}]}}` + "\n\n",
			nested: true,
		},
		{
			name:   "legacy_delta",
			prefix: `data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n",
		},
	} {
		t.Run(delivery.name, func(t *testing.T) {
			for _, tt := range []struct {
				name   string
				output string
				accept bool
			}{
				{"absent", "", true},
				{"empty_array", `[]`, true},
				{"refusal", `[{"type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"No answer"}]}]`, false},
				{"empty_message", `[{"type":"message","role":"assistant","status":"completed","content":[]}]`, false},
				{"reasoning_only", `[{"type":"reasoning","status":"completed"}]`, false},
				{"non_assistant", `[{"type":"message","role":"user","content":[{"type":"output_text","text":"21"}]}]`, false},
				{"null_output", `null`, false},
				{"invalid_output", `{}`, false},
			} {
				t.Run(tt.name, func(t *testing.T) {
					response := map[string]any{
						"status": "completed",
						"usage":  map[string]any{"reasoning_tokens": 1992},
					}
					if tt.output != "" {
						var output any
						require.NoError(t, json.Unmarshal([]byte(tt.output), &output))
						response["output"] = output
					}
					terminal := response
					if delivery.nested {
						terminal = map[string]any{"type": "response.completed", "response": response}
					} else {
						terminal["type"] = "response.completed"
					}
					packet, err := json.Marshal(terminal)
					require.NoError(t, err)
					body := []byte(delivery.prefix + "data: " + string(packet) + "\n\n")
					pattern := openAIDowngradeNumericAnswerPattern(21)
					correct, tokens, juice := parseOpenAIDowngradeProbeResponse(body, pattern)
					require.Equal(t, tt.accept, correct)
					require.Nil(t, juice)
					result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
					result.applyResponse(body, pattern)
					require.Equal(t, tt.accept, result.TransportOK)
					require.Equal(t, tt.accept, result.IsQualificationPass())
					if tt.accept {
						require.NotNil(t, tokens)
						require.Equal(t, 1992, *tokens)
						require.Empty(t, result.ErrorMessage)
						return
					}
					require.Nil(t, tokens)
					require.NotEmpty(t, result.ErrorMessage)
					require.False(t, result.IsRecovered())
					require.False(t, result.IsDegraded(), "invalid evidence must not penalize an account")
					state := OpenAIDowngradeProbeState{
						State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal", ConsecutiveFailures: 1,
					}
					transition := ApplyOpenAIDowngradeProbeResult(state, result, time.Now())
					require.False(t, transition.Circuit)
					require.Equal(t, state.ConsecutiveFailures, transition.State.ConsecutiveFailures)
				})
			}
		})
	}
}

func TestOpenAIDowngradeStreamProbeFallbackIsNarrowlyScoped(t *testing.T) {
	require.True(t, shouldRetryOpenAIDowngradeStreamProbe(
		400, []byte(`{"error":"stream is not supported for this endpoint"}`),
	))
	require.False(t, shouldRetryOpenAIDowngradeStreamProbe(
		429, []byte(`{"error":"stream is not supported for this endpoint"}`),
	))
	require.False(t, shouldRetryOpenAIDowngradeStreamProbe(
		400, []byte(`{"error":"invalid model"}`),
	))
}

func TestProbeResponseRejectsUnqualifiedCompletionEvidence(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"json_failed", `{"status":"failed","output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"json_incomplete", `{"status":"incomplete","output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"json_error", `{"error":{"message":"21"},"usage":{"reasoning_tokens":1992}}`},
		{"delta_only", "event: response.output_text.delta\ndata: {\"delta\":\"21\",\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
		{"done_without_completion", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\",\"usage\":{\"reasoning_tokens\":1992}}\n\ndata: [DONE]\n\n"},
		{"truncated_terminal_frame", "event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}"},
		{"single_newline_is_not_record_end", "event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n"},
		{"failed_terminal", "event: response.failed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
		{"conflicting_event_type", "event: response.completed\ndata: {\"type\":\"response.failed\",\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
		{"invalid_event_type", "event: response.completed\ndata: {\"type\":42,\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
		{"mixed_response", "data: {\"type\":\"response.created\",\"response\":{\"id\":\"first\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"second\",\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}}\n\n"},
		{"fractional_usage", `{"output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":800.9}}`},
		{"negative_usage", `{"output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":-1}}`},
		{"overflow_usage", `{"output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":9223372036854775808}}`},
		{"conflicting_usage", `{"output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992,"output_tokens_details":{"reasoning_tokens":12}}}`},
		{"malformed_usage_details", `{"output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992,"output_tokens_details":"invalid"}}`},
		{"nested_spoofed_usage", `{"output":[{"content":[{"text":"21","metadata":{"reasoning_tokens":1992}}]}]}`},
		{"malformed_response_wrapper", `{"type":"response.completed","response":"invalid","output":[{"content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"incomplete_output_item", `{"status":"completed","output":[{"status":"incomplete","content":[{"text":"21"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"mixed_legacy_delta_lanes", "event: response.output_text.delta\ndata: {\"item_id\":\"one\",\"delta\":\"2\"}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"two\",\"delta\":\"1\"}\n\nevent: response.completed\ndata: {\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
		{"repeated_terminal", "event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\nevent: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"22\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			correct, tokens, juice := parseOpenAIDowngradeProbeResponse([]byte(tt.body), openAIDowngradeNumericAnswerPattern(21))
			require.False(t, correct)
			require.Nil(t, tokens)
			require.Nil(t, juice)
			result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
			result.applyResponse([]byte(tt.body), openAIDowngradeNumericAnswerPattern(21))
			require.False(t, result.TransportOK)
			require.NotEmpty(t, result.ErrorMessage)
			require.False(t, result.IsQualificationPass())
			require.False(t, result.IsRecovered())
			require.False(t, result.IsDegraded(), "invalid evidence must not penalize an account")
			state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty,
				ProbeMode: "normal", ConsecutiveFailures: 1}
			transition := ApplyOpenAIDowngradeProbeResult(state, result, time.Now())
			require.False(t, transition.Circuit)
			require.Equal(t, state.ConsecutiveFailures, transition.State.ConsecutiveFailures)
		})
	}
}

func TestProbeResponseUsesOnlyFinalOutputAndUsage(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"echoed_input", `{"input":[{"content":[{"text":"21"}]}],"output":[{"content":[{"text":"22"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"reasoning_item", `{"output":[{"type":"reasoning","content":[{"text":"21"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"22"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"user_output", `{"output":[{"role":"user","content":[{"text":"21"}]},{"role":"assistant","content":[{"text":"22"}]}],"usage":{"reasoning_tokens":1992}}`},
		{"delta_superseded", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"content\":[{\"text\":\"22\"}]}],\"usage\":{\"output_tokens_details\":{\"reasoning_tokens\":1992}}}}\n\n"},
		{"separate_output_items", `{"output":[{"content":[{"text":"2"}]},{"content":[{"text":"1"}]}],"usage":{"reasoning_tokens":1992}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			correct, tokens, _ := parseOpenAIDowngradeProbeResponse([]byte(tt.body), openAIDowngradeNumericAnswerPattern(21))
			require.False(t, correct)
			require.NotNil(t, tokens)
			require.Equal(t, 1992, *tokens)
		})
	}
}

func TestProbeResponseCompletedEvidencePreservesQualificationBoundary(t *testing.T) {
	for _, tokens := range []int{799, 800, 1399, 1400, 1552, 1992} {
		response := map[string]any{
			"id": "same-response", "status": "completed",
			"output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "21"}}}},
			"usage": map[string]any{"output_tokens_details": map[string]any{"reasoning_tokens": tokens}},
		}
		packet, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		require.NoError(t, err)
		body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"same-response\"}}\r\n\r\n" +
			"event: response.completed\r\ndata: " + string(packet) + "\r\n\r\ndata: [DONE]\r\n\r\n"
		correct, parsed, _ := parseOpenAIDowngradeProbeResponse([]byte(body), openAIDowngradeNumericAnswerPattern(21))
		require.True(t, correct)
		require.NotNil(t, parsed)
		require.Equal(t, tokens, *parsed)
		result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
		result.applyResponse([]byte(body), openAIDowngradeNumericAnswerPattern(21))
		require.True(t, result.TransportOK)
		require.Empty(t, result.ErrorMessage)
		require.Equal(t, tokens >= 800, result.IsQualificationPass())
		require.Equal(t, tokens >= 1400 && !isOpenAIDowngradeTruncationFingerprint(tokens), result.IsRecovered())
	}
}

type downgradeProbeInterruptedReader struct{ data string }

func (r *downgradeProbeInterruptedReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

type downgradeProbeResponseBody struct {
	io.Reader
	closeCalls int
}

func (b *downgradeProbeResponseBody) Close() error {
	b.closeCalls++
	return nil
}

type downgradeProbeHTTPUpstream struct {
	HTTPUpstream
	doProbe func() (*http.Response, error)
}

func (u *downgradeProbeHTTPUpstream) DoProbeWithTLS(
	_ *http.Request, _ string, _ int, _ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.doProbe()
}

func newDowngradeProbeHTTPTestRunner(upstream HTTPUpstream) (*OpenAIDowngradeProbeRunner, *Account) {
	proxyID := int64(3)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		ProxyID: &proxyID, Concurrency: 50,
		Proxy:       &Proxy{ID: proxyID, Protocol: "http", Host: "127.0.0.1", Port: 3128, Status: StatusActive},
		Credentials: map[string]any{"access_token": "synthetic-probe-test-value"},
	}
	// Direct construction avoids rebinding the process-wide telemetry manager.
	return &OpenAIDowngradeProbeRunner{
		store: &downgradeProbeStoreStub{}, accountRepo: &downgradeProbeAccountRepoStub{account: account},
		tokenProvider: NewOpenAITokenProvider(nil, nil, nil), httpUpstream: upstream,
	}, account
}

func TestOpenAIDowngradeProbeBodyFailurePreservesHTTPEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		bodyKind   string
		reset      bool
		errorClass string
	}{
		{"429_reset_interrupted", http.StatusTooManyRequests, "interrupted", true, "connection interrupted"},
		{"429_no_reset_interrupted", http.StatusTooManyRequests, "interrupted", false, "connection interrupted"},
		{"429_reset_oversized", http.StatusTooManyRequests, "oversized", true, "response body exceeds limit"},
		{"429_no_reset_oversized", http.StatusTooManyRequests, "oversized", false, "response body exceeds limit"},
		{"401_interrupted", http.StatusUnauthorized, "interrupted", false, "connection interrupted"},
		{"403_oversized", http.StatusForbidden, "oversized", false, "response body exceeds limit"},
		{"200_interrupted", http.StatusOK, "interrupted", false, "connection interrupted"},
		{"200_oversized", http.StatusOK, "oversized", false, "response body exceeds limit"},
		{"200_nil_body", http.StatusOK, "nil", false, "response body unavailable"},
		{"200_partial_completion", http.StatusOK, "partial", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			if tc.reset {
				header.Set("x-codex-primary-window-minutes", "10080")
				header.Set("x-codex-primary-used-percent", "100")
				header.Set("x-codex-primary-reset-after-seconds", "259200")
			}
			var body *downgradeProbeResponseBody
			response := &http.Response{StatusCode: tc.status, Header: header}
			if tc.bodyKind != "nil" {
				body = &downgradeProbeResponseBody{Reader: iotest.ErrReader(
					fmt.Errorf("private-network-marker: %w", io.ErrUnexpectedEOF))}
				if tc.bodyKind == "oversized" {
					body.Reader = strings.NewReader(strings.Repeat("x", openAIDowngradeProbeMaxBodyBytes+1))
				} else if tc.bodyKind == "partial" {
					body.Reader = &downgradeProbeInterruptedReader{
						data: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"",
					}
				}
				response.Body = body
			}
			calls := 0
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{
				doProbe: func() (*http.Response, error) {
					calls++
					return response, nil
				},
			})
			now := time.Now()
			if tc.status == http.StatusOK {
				limitedAt, resetAt := now.Add(-time.Hour), now.Add(time.Hour)
				account.RateLimitedAt, account.RateLimitResetAt = &limitedAt, &resetAt
			}
			result := runner.probe(context.Background(), account, "qualification")
			require.Equal(t, 1, calls)
			if body != nil {
				require.Equal(t, 1, body.closeCalls)
			}
			require.Equal(t, tc.status, result.HTTPStatus)
			if tc.bodyKind == "partial" {
				require.Equal(t, "probe response missing valid completion or reasoning usage", result.ErrorMessage)
			} else {
				require.Equal(t, "probe transport failed: "+tc.errorClass, result.ErrorMessage)
			}
			require.False(t, result.TransportOK)
			require.False(t, result.IsQualificationPass())
			require.False(t, result.IsRecovered())
			require.False(t, result.IsDegraded())
			require.Nil(t, result.ReasoningTokens)
			if tc.reset {
				require.NotNil(t, result.RateLimitResetAt)
				require.WithinDuration(t, now.Add(72*time.Hour), *result.RateLimitResetAt, 5*time.Second)
				require.Equal(t, "7d_window", result.RateLimitWindow)
			} else {
				require.Nil(t, result.RateLimitResetAt)
			}

			require.NoError(t, runner.recordProbeResult(context.Background(), &result))
			store := runner.store.(*downgradeProbeStoreStub)
			require.Equal(t, []OpenAIDowngradeProbeResult{result}, store.probeResults)
			repo := runner.accountRepo.(*downgradeProbeAccountRepoStub)
			// r17aq：401/403 不再在记录入口一击 SetError——降级不杀策略在
			// 状态机侧（applyOpenAIProbeAuthPolicy），记录路径零生命周期副作用。
			require.Empty(t, repo.errorMessages)
			state := &OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification",
				ConsecutiveFailures: 1, ConsecutiveSuccesses: 3,
				Consecutive429s: openAIDowngrade429StreakThreshold - 1,
			}
			transition := ApplyOpenAIDowngradeProbeResult(*state, result, now)
			require.Equal(t, 1, transition.State.ConsecutiveFailures)
			require.Equal(t, 3, transition.State.ConsecutiveSuccesses)
			handled, err := runner.applyRateLimitDeferral(context.Background(), account, state, result, now)
			require.NoError(t, err)
			require.Equal(t, tc.status == http.StatusTooManyRequests, handled)
			require.Empty(t, repo.openAIRateLimitClears)
			if tc.status == http.StatusTooManyRequests {
				require.Equal(t, 1, store.saveCalls)
				require.True(t, state.NextProbeAt.After(now.Add(50*time.Minute)))
				if tc.reset {
					require.Equal(t, []time.Time{*result.RateLimitResetAt}, repo.rateLimitedResets)
				} else {
					require.Equal(t, openAIDowngrade429StreakThreshold, state.Consecutive429s)
					require.Equal(t, []time.Time{state.NextProbeAt}, repo.rateLimitedResets)
				}
			}
		})
	}
}

func TestOpenAIDowngradeProbeCompleteBodyRemainsUsable(t *testing.T) {
	complete := "event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted_%t", interrupted), func(t *testing.T) {
			body := &downgradeProbeResponseBody{Reader: strings.NewReader(complete)}
			if interrupted {
				body.Reader = &downgradeProbeInterruptedReader{data: complete}
			}
			calls := 0
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{
				doProbe: func() (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
				},
			})
			result := runner.probe(context.Background(), account, "normal")
			require.Equal(t, 1, calls)
			require.Equal(t, 1, body.closeCalls)
			require.Equal(t, http.StatusOK, result.HTTPStatus)
			require.True(t, result.TransportOK)
			require.Equal(t, downgradeProbeIntPtr(1992), result.ReasoningTokens)
			require.Empty(t, result.ErrorMessage)
		})
	}
}

func TestOpenAIDowngradeProbeRetryFailureUsesFinalAttempt(t *testing.T) {
	for _, transportFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("transport_failure_%t", transportFailure), func(t *testing.T) {
			firstBody := &downgradeProbeResponseBody{Reader: strings.NewReader("stream unsupported")}
			lastBody := &downgradeProbeResponseBody{Reader: iotest.ErrReader(io.ErrUnexpectedEOF)}
			header := make(http.Header)
			header.Set("x-codex-primary-window-minutes", "10080")
			header.Set("x-codex-primary-used-percent", "100")
			header.Set("x-codex-primary-reset-after-seconds", "259200")
			calls := 0
			runner, account := newDowngradeProbeHTTPTestRunner(&downgradeProbeHTTPUpstream{
				doProbe: func() (*http.Response, error) {
					calls++
					if calls == 1 {
						return &http.Response{StatusCode: http.StatusBadRequest, Body: firstBody}, nil
					}
					last := &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: lastBody}
					if transportFailure {
						return last, fmt.Errorf("private-network-marker: %w", context.DeadlineExceeded)
					}
					return last, nil
				},
			})
			result := runner.probe(context.Background(), account, "normal")
			require.Equal(t, 2, calls)
			require.Equal(t, 1, firstBody.closeCalls)
			require.Equal(t, 1, lastBody.closeCalls)
			require.False(t, result.TransportOK)
			if transportFailure {
				// A response returned alongside a transport error is not accepted evidence.
				require.Zero(t, result.HTTPStatus)
				require.Nil(t, result.RateLimitResetAt)
				require.Equal(t, "probe stream retry failed: timeout", result.ErrorMessage)
			} else {
				require.Equal(t, http.StatusTooManyRequests, result.HTTPStatus)
				require.NotNil(t, result.RateLimitResetAt)
				require.Equal(t, "7d_window", result.RateLimitWindow)
				require.Equal(t, "probe stream retry failed: connection interrupted", result.ErrorMessage)
			}
			require.NotContains(t, result.ErrorMessage, "private-network-marker")
		})
	}
}

func TestOpenAIDowngradeProbeErrorClassesExcludeRawDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{&net.DNSError{IsTimeout: true}, "timeout"},
		{io.EOF, "connection interrupted"},
		{io.ErrUnexpectedEOF, "connection interrupted"},
		{net.ErrClosed, "connection interrupted"},
		{syscall.ECONNRESET, "connection interrupted"},
		{syscall.EPIPE, "connection interrupted"},
		{errOpenAIDowngradeProbeBodyUnavailable, "response body unavailable"},
		{errOpenAIDowngradeProbeBodyTooLarge, "response body exceeds limit"},
		{errors.New("private-network-marker"), "network or response error"},
	} {
		t.Run(tc.want+"/"+fmt.Sprintf("%T", tc.err), func(t *testing.T) {
			err := fmt.Errorf("private-network-marker: %w", tc.err)
			require.Equal(t, tc.want, openAIDowngradeProbeErrorClass(err))
		})
	}
}

func TestProbeResponseBodyLimitAndInterruptedCompletion(t *testing.T) {
	complete := "event: response.completed\ndata: {\"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}\n\n"
	for _, tt := range []struct {
		name   string
		reader io.Reader
		err    bool
		valid  bool
	}{
		{"nil_body", nil, true, false},
		{"empty_interrupted", &downgradeProbeInterruptedReader{}, true, false},
		{"partial_interrupted", &downgradeProbeInterruptedReader{data: strings.TrimSuffix(complete, "\n")}, false, false},
		{"complete_then_disconnect", &downgradeProbeInterruptedReader{data: complete}, false, true},
		{"exact_limit", strings.NewReader(complete + ":" + strings.Repeat("x", openAIDowngradeProbeMaxBodyBytes-len(complete)-1)), false, true},
		{"over_limit", strings.NewReader(complete + ":" + strings.Repeat("x", openAIDowngradeProbeMaxBodyBytes-len(complete))), true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := readOpenAIDowngradeProbeBody(tt.reader)
			if tt.err {
				require.Error(t, err)
				require.Nil(t, body)
				return
			}
			require.NoError(t, err)
			result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
			result.applyResponse(body, openAIDowngradeNumericAnswerPattern(21))
			require.Equal(t, tt.valid, result.IsQualificationPass())
			require.False(t, result.IsDegraded())
		})
	}
	result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
	result.applyResponse([]byte(complete), nil)
	require.False(t, result.TransportOK)
	require.False(t, result.IsDegraded())
}

func TestProbeResponseSSEMultilineAndReplayReset(t *testing.T) {
	body := ": keepalive\r\nid: local-event\r\nevent: response.completed\r\n" +
		"data: {\"response\":{\"status\":\"completed\",\r\n" +
		"data: \"output\":[{\"content\":[{\"text\":\"21\"}]}],\"usage\":{\"reasoning_tokens\":1992}}}\r\n\r\n"
	result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusOK}
	result.applyResponse([]byte(body), openAIDowngradeNumericAnswerPattern(21))
	require.True(t, result.IsQualificationPass())
	result.applyResponse([]byte("data: {\"type\":\"response.completed\"}\n"), openAIDowngradeNumericAnswerPattern(21))
	require.False(t, result.IsQualificationPass())
	require.False(t, result.IsDegraded())
	require.Nil(t, result.ReasoningTokens)
}

func TestOpenAIDowngradeProbeInconclusiveDoesNotMoveState(t *testing.T) {
	state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty}
	inconclusive := OpenAIDowngradeProbeResult{
		TransportOK:   true,
		AnswerCorrect: true,
	}

	transition := ApplyOpenAIDowngradeProbeResult(state, inconclusive, time.Now())
	require.Equal(t, OpenAIDowngradeStateOnDuty, transition.NextState)
	require.False(t, transition.Circuit)
	require.False(t, transition.Recovered)
}

func (s *downgradeProbeStoreStub) CanRunOpenAIDowngradeProbe(context.Context, int64) (bool, error) {
	return true, nil
}

func waitForDowngradeProbeShutdown(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not stop promptly")
	}
}

func TestOpenAIDowngradeProbeStopBeforeStart(t *testing.T) {
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	stopped := make(chan struct{})
	go func() {
		runner.Stop()
		close(stopped)
	}()
	waitForDowngradeProbeShutdown(t, stopped)
	runner.Start()
	runner.Start()
	runner.Stop()
	waitForDowngradeProbeShutdown(t, runner.doneCh)
	require.ErrorIs(t, runner.RunOnce(context.Background()), context.Canceled)
	require.Zero(t, store.probeCalls)
	require.Zero(t, store.saveCalls)
	require.True(t, store.ensureNextAt.IsZero())
}

func TestOpenAIDowngradeProbeConcurrentStartStop(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(
		&downgradeProbeStoreStub{}, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
	start := make(chan struct{})
	results := make(chan error, 32)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func(operation int) {
			defer workers.Done()
			<-start
			switch operation {
			case 0:
				runner.Start()
			case 1:
				runner.Stop()
			default:
				results <- runner.RunOnce(context.Background())
			}
		}(i % 3)
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	close(start)
	waitForDowngradeProbeShutdown(t, done)
	close(results)
	for err := range results {
		require.True(t, err == nil || errors.Is(err, context.Canceled))
	}
	waitForDowngradeProbeShutdown(t, runner.doneCh)
	require.ErrorIs(t, runner.RunOnce(context.Background()), context.Canceled)
}

func TestOpenAIDowngradeProbeStopCancelsAndWaitsForScan(t *testing.T) {
	for _, phase := range []string{"accounts", "ensure", "reconcile", "due", "state"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan context.Context, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var calls []string
			visit := func(ctx context.Context, current string) {
				calls = append(calls, current)
				if current == phase {
					entered <- ctx
					<-ctx.Done()
					<-release
				}
			}
			proxyID := int64(3)
			accounts := []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ProxyID: &proxyID},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ProxyID: &proxyID},
			}
			repo := &downgradeProbeAccountRepoStub{
				listByPlatformFn: func(ctx context.Context, _ string) ([]Account, error) {
					visit(ctx, "accounts")
					return accounts, nil
				},
				getByIDFn: func(ctx context.Context, _ int64) (*Account, error) {
					visit(ctx, "state")
					return nil, ctx.Err()
				},
			}
			store := &downgradeProbeStoreStub{
				ensureFn: func(ctx context.Context, _ int64, _ *int64, _ time.Time) (*OpenAIDowngradeProbeState, error) {
					visit(ctx, "ensure")
					return nil, nil
				},
				reconcileFn: func(ctx context.Context, _ time.Time, _ time.Duration) (int64, error) {
					visit(ctx, "reconcile")
					return 0, nil
				},
				listDueFn: func(ctx context.Context, _ time.Time, _ int) ([]OpenAIDowngradeProbeState, error) {
					visit(ctx, "due")
					return []OpenAIDowngradeProbeState{
						{AccountID: 1, State: OpenAIDowngradeStateOnDuty},
						{AccountID: 2, State: OpenAIDowngradeStateOnDuty},
					}, nil
				},
			}
			atomicStore := &downgradeAtomicStoreStub{downgradeProbeStoreStub: store, accountRepo: repo}
			runner := NewOpenAIDowngradeProbeRunner(atomicStore, repo, nil, nil, nil, nil)
			t.Cleanup(runner.Stop)
			// Cover a manual scan both with and without a background loop.
			if phase == "state" {
				runner.Start()
			}
			result := make(chan error, 1)
			go func() { result <- runner.RunOnce(context.Background()) }()
			var scanCtx context.Context
			select {
			case scanCtx = <-entered:
			case <-time.After(2 * time.Second):
				runner.Stop()
				t.Fatal("scan did not reach the selected phase")
			}
			// An overlapping caller must neither start work nor cancel the owner.
			require.NoError(t, runner.RunOnce(context.Background()))
			require.NoError(t, scanCtx.Err())
			stopped := make(chan struct{})
			go func() {
				runner.Stop()
				close(stopped)
			}()
			waitForDowngradeProbeShutdown(t, scanCtx.Done())
			select {
			case <-stopped:
				t.Fatal("Stop returned while the scan still held a dependency")
			default:
			}
			unblock()
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(2 * time.Second):
				t.Fatal("canceled scan did not return")
			}
			waitForDowngradeProbeShutdown(t, stopped)
			expected := map[string][]string{
				"accounts":  {"accounts"},
				"ensure":    {"accounts", "ensure"},
				"reconcile": {"accounts", "ensure", "ensure", "reconcile"},
				"due":       {"accounts", "ensure", "ensure", "reconcile", "due"},
				"state":     {"accounts", "ensure", "ensure", "reconcile", "due", "state"},
			}
			require.Equal(t, expected[phase], calls)
			require.Zero(t, store.probeCalls)
			require.Zero(t, store.saveCalls)
			require.Zero(t, atomicStore.commits)
			require.ErrorIs(t, runner.RunOnce(context.Background()), context.Canceled)
		})
	}
}

func TestOpenAIDowngradeProbeCanceledCallerDoesNotStopRunner(t *testing.T) {
	calls := 0
	repo := &downgradeProbeAccountRepoStub{
		listByPlatformFn: func(context.Context, string) ([]Account, error) {
			calls++
			return nil, nil
		},
	}
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, repo, nil, nil, nil, nil)
	defer runner.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, runner.RunOnce(ctx), context.Canceled)
	require.Zero(t, calls)
	require.NoError(t, runner.RunOnce(context.Background()))
	require.Equal(t, 1, calls)
	runner.lifecycleMu.Lock()
	hasCancel, hasDone := runner.runCancel != nil, runner.runDone != nil
	runner.lifecycleMu.Unlock()
	require.False(t, hasCancel)
	require.False(t, hasDone)
}

type downgradeProbePurgeStoreStub struct {
	*downgradeProbeStoreStub
	purgeFn    func(context.Context, time.Time, time.Time) (int64, int64, error)
	goneFn     func(context.Context) (int64, error)
	purgeCalls int
	goneCalls  int
}

func (s *downgradeProbePurgeStoreStub) PurgeOpenAIDowngradeProbeHistory(ctx context.Context, resultsBefore, eventsBefore time.Time) (int64, int64, error) {
	s.purgeCalls++
	if s.purgeFn != nil {
		return s.purgeFn(ctx, resultsBefore, eventsBefore)
	}
	return 0, 0, nil
}

func (s *downgradeProbePurgeStoreStub) DeleteOpenAIDowngradeStatesForGoneAccounts(ctx context.Context) (int64, error) {
	s.goneCalls++
	if s.goneFn != nil {
		return s.goneFn(ctx)
	}
	return 0, nil
}

func TestOpenAIDowngradeProbeCanceledPurgeDoesNotContinueCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &downgradeProbePurgeStoreStub{
		downgradeProbeStoreStub: &downgradeProbeStoreStub{},
		purgeFn: func(context.Context, time.Time, time.Time) (int64, int64, error) {
			cancel()
			return 0, 0, nil
		},
	}
	listCalls := 0
	repo := &downgradeProbeAccountRepoStub{
		listByPlatformFn: func(context.Context, string) ([]Account, error) {
			listCalls++
			return nil, nil
		},
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	defer runner.Stop()
	require.ErrorIs(t, runner.RunOnce(ctx), context.Canceled)
	require.Zero(t, store.goneCalls)
	require.Zero(t, listCalls)
	require.True(t, runner.lastPurgeAt.IsZero())
}

func TestOpenAIDowngradeProbeCleanupFailureDoesNotConsumeDailySchedule(t *testing.T) {
	for _, phase := range []string{"history", "gone"} {
		for _, failure := range []string{"error", "canceled"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
				lastSuccess := now.Add(-25 * time.Hour)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				fail := func() error {
					if failure == "canceled" {
						cancel()
						return nil
					}
					return errors.New("cleanup unavailable")
				}
				store := &downgradeProbePurgeStoreStub{
					downgradeProbeStoreStub: &downgradeProbeStoreStub{},
				}
				store.purgeFn = func(_ context.Context, resultsBefore, eventsBefore time.Time) (int64, int64, error) {
					require.Equal(t, now.Add(-openAIDowngradeResultsRetention), resultsBefore)
					require.Equal(t, now.Add(-openAIDowngradeEventsRetention), eventsBefore)
					if phase == "history" && store.purgeCalls == 1 {
						return 0, 0, fail()
					}
					return 0, 0, nil
				}
				store.goneFn = func(context.Context) (int64, error) {
					if phase == "gone" && store.goneCalls == 1 {
						return 0, fail()
					}
					return 0, nil
				}
				runner := NewOpenAIDowngradeProbeRunner(
					store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
				defer runner.Stop()
				runner.now = func() time.Time { return now }
				runner.lastPurgeAt = lastSuccess
				err := runner.RunOnce(ctx)
				if failure == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, lastSuccess, runner.lastPurgeAt)
				retryAt := now.Add(openAIDowngradePurgeRetryInterval)
				require.Equal(t, retryAt, runner.purgeRetryAt)
				require.Equal(t, 1, store.purgeCalls)
				expectedGoneCalls := 0
				if phase == "gone" || failure == "error" {
					expectedGoneCalls = 1
				}
				require.Equal(t, expectedGoneCalls, store.goneCalls)

				now = retryAt.Add(-time.Nanosecond)
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 1, store.purgeCalls)
				require.Equal(t, expectedGoneCalls, store.goneCalls)
				now = retryAt
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 2, store.purgeCalls)
				require.Equal(t, expectedGoneCalls+1, store.goneCalls)
				require.Equal(t, now, runner.lastPurgeAt)
				require.True(t, runner.purgeRetryAt.IsZero())

				nextDaily := now.Add(openAIDowngradePurgeInterval)
				now = nextDaily.Add(-time.Nanosecond)
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 2, store.purgeCalls)
				require.Equal(t, expectedGoneCalls+1, store.goneCalls)
				now = nextDaily
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 3, store.purgeCalls)
				require.Equal(t, expectedGoneCalls+2, store.goneCalls)
				require.Equal(t, now, runner.lastPurgeAt)
			})
		}
	}
}

func TestOpenAIDowngradeProbeCleanupSchedulesFromCompletion(t *testing.T) {
	for _, phase := range []string{"history", "gone"} {
		for _, outcome := range []string{"success", "error", "canceled"} {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				startedAt := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
				now := startedAt
				lastSuccess := now.Add(-25 * time.Hour)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				finish := func() error {
					now = now.Add(6 * time.Minute)
					switch outcome {
					case "error":
						return errors.New("cleanup unavailable")
					case "canceled":
						cancel()
					}
					return nil
				}
				store := &downgradeProbePurgeStoreStub{
					downgradeProbeStoreStub: &downgradeProbeStoreStub{},
				}
				store.purgeFn = func(_ context.Context, resultsBefore, eventsBefore time.Time) (int64, int64, error) {
					require.Equal(t, startedAt.Add(-openAIDowngradeResultsRetention), resultsBefore)
					require.Equal(t, startedAt.Add(-openAIDowngradeEventsRetention), eventsBefore)
					if phase == "history" {
						return 0, 0, finish()
					}
					return 0, 0, nil
				}
				store.goneFn = func(context.Context) (int64, error) {
					if phase == "gone" {
						return 0, finish()
					}
					return 0, nil
				}
				runner := NewOpenAIDowngradeProbeRunner(
					store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
				defer runner.Stop()
				runner.now = func() time.Time { return now }
				runner.lastPurgeAt = lastSuccess
				err := runner.RunOnce(ctx)
				if outcome == "canceled" {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, startedAt.Add(6*time.Minute), now)
				nextAt := now.Add(openAIDowngradePurgeRetryInterval)
				if outcome == "success" {
					require.Equal(t, now, runner.lastPurgeAt)
					require.True(t, runner.purgeRetryAt.IsZero())
					nextAt = now.Add(openAIDowngradePurgeInterval)
				} else {
					require.Equal(t, lastSuccess, runner.lastPurgeAt)
					require.Equal(t, nextAt, runner.purgeRetryAt)
				}
				goneCalls := store.goneCalls
				store.purgeFn = nil
				store.goneFn = nil
				now = nextAt.Add(-time.Nanosecond)
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 1, store.purgeCalls)
				require.Equal(t, goneCalls, store.goneCalls)
				now = nextAt
				require.NoError(t, runner.RunOnce(context.Background()))
				require.Equal(t, 2, store.purgeCalls)
				require.Equal(t, goneCalls+1, store.goneCalls)
				require.Equal(t, now, runner.lastPurgeAt)
				require.True(t, runner.purgeRetryAt.IsZero())
			})
		}
	}
}

func TestOpenAIDowngradeProbeCleanupOptionalCapabilities(t *testing.T) {
	for _, capability := range []string{"none", "history", "gone", "both"} {
		t.Run(capability, func(t *testing.T) {
			now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
			base := &downgradeProbeStoreStub{}
			cleaner := &downgradeProbePurgeStoreStub{downgradeProbeStoreStub: base}
			var store OpenAIDowngradeProbeStore = base
			switch capability {
			case "history":
				store = &struct {
					*downgradeProbeStoreStub
					OpenAIDowngradeProbeHistoryCleaner
				}{base, cleaner}
			case "gone":
				store = &struct {
					*downgradeProbeStoreStub
					OpenAIDowngradeGoneAccountStateCleaner
				}{base, cleaner}
			case "both":
				store = cleaner
			}
			runner := NewOpenAIDowngradeProbeRunner(
				store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
			defer runner.Stop()
			runner.now = func() time.Time { return now }
			require.NoError(t, runner.RunOnce(context.Background()))
			if capability == "none" {
				require.True(t, runner.lastPurgeAt.IsZero())
			} else {
				require.Equal(t, now, runner.lastPurgeAt)
			}
			require.True(t, runner.purgeRetryAt.IsZero())
			// Successful capabilities must not execute again in the same interval.
			require.NoError(t, runner.RunOnce(context.Background()))
			historyCalls, goneCalls := 0, 0
			if capability == "history" || capability == "both" {
				historyCalls = 1
			}
			if capability == "gone" || capability == "both" {
				goneCalls = 1
			}
			require.Equal(t, historyCalls, cleaner.purgeCalls)
			require.Equal(t, goneCalls, cleaner.goneCalls)
		})
	}
}

func TestOpenAIDowngradeProbeInitialScheduleIsNotRewrittenEveryRun(t *testing.T) {
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	proxyID := int64(3)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return 12 * time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true}
	}

	require.NoError(t, runner.RunOnce(context.Background()))
	require.Equal(t, now.Add(12*time.Minute), store.ensureNextAt)
	require.Zero(t, store.probeCalls)
	require.Zero(t, store.saveCalls)
}

func TestOpenAIDowngradeProbeRejectsTerminalReplacementWithoutProxy(t *testing.T) {
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		store.probeCalls++
		return OpenAIDowngradeProbeResult{}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStatePendingReplace,
		ProbeMode: "normal",
	}

	// 普通 pending_replace 不再走 retryReplacement/beginReprobe（原实现无
	// proxy 时报错），而是防御性让位：不探针、排远 7 天、零 probe 调用。
	// 打票线已删除（2026-10-02）：pending_replace 无探针例外，救治区/手动启用是唯一救援入口。
	require.NoError(t, runner.processState(context.Background(), state, time.Now()))
	require.Zero(t, store.probeCalls)
	require.True(t, state.NextProbeAt.After(time.Now().Add(6*24*time.Hour)),
		"判死号必须被排远(防御性让位),不是近刻重探")
}

func TestOpenAIDowngradeProbeTransportFailureDoesNotCountAsDegradation(t *testing.T) {
	state := OpenAIDowngradeProbeState{
		State: OpenAIDowngradeStateOnDuty, ConsecutiveFailures: 1,
	}
	transportFailure := OpenAIDowngradeProbeResult{
		HTTPStatus: 429, ErrorMessage: "rate limited",
	}

	transition := ApplyOpenAIDowngradeProbeResult(state, transportFailure, time.Now())
	require.False(t, transition.Circuit)
	require.Equal(t, 1, transition.State.ConsecutiveFailures)
}

func TestOpenAIDowngradeProbeRateLimitedSchedulesShortRetry(t *testing.T) {
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			HTTPStatus: http.StatusTooManyRequests, ErrorMessage: "rate limited",
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls)
	// 429 后走短周期重探（jitter 后 0.5x-1.5x），不再落入 15-45 分钟常规排期。
	lo := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 0.5))
	hi := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 1.5))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"rate-limited retry %v outside short-cycle window [%v, %v]",
		state.NextProbeAt, lo, hi)
}

func TestOpenAIDowngradeProbeSpreadKeepsMinimumAndDesynchronizes(t *testing.T) {
	runner := NewOpenAIDowngradeProbeRunner(nil, nil, nil, nil, nil, nil)
	base := openAIDowngradeReplacementWindow
	distinct := make(map[time.Duration]struct{})
	for i := 0; i < 50; i++ {
		d := runner.spread(base)
		require.False(t, d < base, "spread shortened a deadline-style interval: %v < %v", d, base)
		max := time.Duration(float64(base) * 1.25)
		require.False(t, d > max, "spread exceeded cap: %v > %v", d, max)
		distinct[d] = struct{}{}
	}
	require.Greater(t, len(distinct), 10,
		"spread failed to desynchronize: %d distinct durations in 50 draws", len(distinct))
}

func TestOpenAIDowngradeProbeSchedulerWriteFailureDoesNotPersistCircuit(t *testing.T) {
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Priority: 23,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{
		account: account, schedulableErr: errors.New("scheduler write failed"),
	}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(516),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty,
		ConsecutiveFailures: 1, CurrentProxyID: nil, OriginalProxyID: nil,
	}

	err := runner.processState(context.Background(), state, time.Now())
	require.Error(t, err)
	require.Equal(t, []bool{false}, repo.schedulableCalls)
	require.Equal(t, 23, account.Priority)
	require.Zero(t, store.saveCalls)
	require.Zero(t, store.eventCalls)
}

func TestOpenAIDowngradeProbeQualificationPromotesAfterFirstHealthyProbe(t *testing.T) {
	// 2026-09-15 用户裁定「新号一次检测合格就可以上岗，不要整那么多次」：
	// 1 针通过即解锁 schedulable；上岗后由 on_duty 常规随机节奏持续动态监测。
	mainProxyID := int64(3)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &mainProxyID,
		Extra: map[string]any{openAIDowngradeQualificationExtraKey: true},
	}
	store := &downgradeProbeStoreStub{mainProxyID: &mainProxyID}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return now }
	runner.nextDelay = func() time.Duration { return time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "qualification", NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.True(t, account.Schedulable, "single qualification pass must promote")
	require.Equal(t, 1, store.probeCalls)
	require.Empty(t, store.proxyChanges, "qualification must retain the authorization route")
	require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
	require.Equal(t, "normal", state.ProbeMode)
	require.Equal(t, 0, state.ConsecutiveSuccesses, "promotion resets the streak")

	// 中途持续动态监测：上岗后走常规随机节奏（nextDelay 桩=1min），
	// 不再落资格 5min 档，解锁状态稳定不回退。
	account.ProxyID = &mainProxyID
	now = now.Add(time.Minute)
	require.NoError(t, runner.processState(context.Background(), state, now))
	require.True(t, account.Schedulable)
	require.Equal(t, 2, store.probeCalls)
	require.Equal(t, "normal", state.ProbeMode)
	require.Equal(t, now.Add(time.Minute), state.NextProbeAt)
}

func TestOpenAIDowngradeProbeQualificationFailureKeepsAccountPaused(t *testing.T) {
	mainProxyID := int64(3)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &mainProxyID,
		Extra: map[string]any{openAIDowngradeQualificationExtraKey: true},
	}
	store := &downgradeProbeStoreStub{mainProxyID: &mainProxyID}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: false,
			ReasoningTokens: downgradeProbeIntPtr(516),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "qualification", NextProbeAt: time.Now(),
		ConsecutiveFailures: 1,
		CurrentProxyID:      &mainProxyID, OriginalProxyID: &mainProxyID,
	}

	require.NoError(t, runner.processState(context.Background(), state, time.Now()))
	require.False(t, account.Schedulable)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, state.State)
	require.Equal(t, 2, state.ConsecutiveFailures)
	require.Equal(t, 1, store.probeCalls)
}

func TestOpenAIDowngradeProbeCircuitWaitsThenRetriesSameRoute(t *testing.T) {
	openedAt := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	now := openedAt.Add(10 * time.Minute)
	originalProxyID := int64(3)
	escapeProxyID := int64(5)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &originalProxyID,
	}
	store := &downgradeProbeStoreStub{escapeProxyID: &escapeProxyID}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateCircuitOpen,
		ProbeMode: "normal", OriginalProxyID: &originalProxyID,
		CurrentProxyID: &originalProxyID, CircuitOpenedAt: &openedAt,
		NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Zero(t, store.probeCalls)
	require.Empty(t, store.proxyChanges)
	// spread 正向散布：冷却窗口不短于 30 分钟，回访散布在其后 0-25% 内。
	minNext := openedAt.Add(openAIDowngradeRecoveryWindow)
	maxNext := openedAt.Add(time.Duration(float64(openAIDowngradeRecoveryWindow) * 1.25))
	require.True(t, !state.NextProbeAt.Before(minNext) && !state.NextProbeAt.After(maxNext),
		"circuit quiet-window revisit %v outside spread window [%v, %v]",
		state.NextProbeAt, minNext, maxNext)

	now = maxNext
	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls)
	require.Equal(t, OpenAIDowngradeStateReprobe, state.State)
	require.Empty(t, store.proxyChanges)
	require.Equal(t, &originalProxyID, state.CurrentProxyID)
}

func TestOpenAIDowngradeSolFallbackRestrictsSchedulingToSolFamily(t *testing.T) {
	account := &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{OpenAIDowngradeSolFallbackExtraKey: true},
	}

	require.True(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "gpt-5.6-sol", false, "",
	))
	require.True(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "gpt-5.6-sol-20260901", false, "",
	))
	require.False(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "gpt-6-astra", false, "",
	))
	require.False(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "", false, "",
	))
	require.False(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "unknown-model", false, "",
	))

	account.Extra[OpenAIDowngradeSolFallbackExtraKey] = false
	require.True(t, isOpenAICompatibleAccountEligibleForRequestBeforeProfit(
		context.Background(), account, PlatformOpenAI, "gpt-6-astra", false, "",
	))
}

func TestOpenAIDowngradeSolFallbackModePersistsOnAccountTransition(t *testing.T) {
	account := &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: false,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.nextDelay = func() time.Duration { return time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK:     true,
			AnswerCorrect:   true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	state := &OpenAIDowngradeProbeState{
		AccountID:        1,
		State:            OpenAIDowngradeStateCircuitOpen,
		ProbeMode:        "half_open",
		CurrentProxyID:   nil,
		OriginalProxyID:  nil,
		RecoveryDeadline: timePtr(now),
		NextProbeAt:      now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, []bool{true}, repo.fallbackModes)
	require.True(t, account.Schedulable)
	require.Equal(t, true, account.Extra[OpenAIDowngradeSolFallbackExtraKey])
}

func TestOpenAIDowngradeSolFallbackClearsAfterTwoAstraRecoveries(t *testing.T) {
	account := &Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{OpenAIDowngradeSolFallbackExtraKey: true},
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK:     true,
			AnswerCorrect:   true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	probeAt := now.Add(-time.Minute)
	state := &OpenAIDowngradeProbeState{
		AccountID:                 1,
		State:                     OpenAIDowngradeStateOnDuty,
		ProbeMode:                 "sol_fallback",
		AstraNextProbeAt:          &probeAt,
		AstraConsecutiveSuccesses: 1,
		NextProbeAt:               now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, []bool{false}, repo.fallbackModes)
	require.Equal(t, "normal", state.ProbeMode)
	require.Nil(t, state.AstraNextProbeAt)
	require.Equal(t, false, account.Extra[OpenAIDowngradeSolFallbackExtraKey])
}

func TestOpenAIDowngradeSolFallbackAstraRecheckAdvancesSchedule(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	resetAt := now.Add(48 * time.Hour)
	recovered := OpenAIDowngradeProbeResult{
		HTTPStatus: http.StatusOK, TransportOK: true, AnswerCorrect: true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
	}
	for _, tc := range []struct {
		name          string
		result        OpenAIDowngradeProbeResult
		successes     int
		streak        int
		wantSuccesses int
		wantFailures  int
		wantNormal    bool
		minDelay      time.Duration
		maxDelay      time.Duration
	}{
		{name: "first_recovery", result: recovered, wantSuccesses: 1},
		{name: "return_to_astra", result: recovered, successes: 1, wantNormal: true},
		{name: "degraded", successes: 1, wantFailures: 1,
			result: OpenAIDowngradeProbeResult{
				HTTPStatus: http.StatusOK, TransportOK: true,
				ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
			}},
		{name: "neutral_fingerprint", successes: 1, wantSuccesses: 1,
			result: OpenAIDowngradeProbeResult{
				HTTPStatus: http.StatusOK, TransportOK: true, AnswerCorrect: true,
				ReasoningTokens: downgradeProbeIntPtr(1552),
			}},
		{name: "transport_failure", successes: 1, wantSuccesses: 1},
		{name: "upstream_failure", successes: 1, wantSuccesses: 1,
			result: OpenAIDowngradeProbeResult{HTTPStatus: http.StatusServiceUnavailable}},
		{name: "short_429", successes: 1, wantSuccesses: 1,
			result:   OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests},
			minDelay: openAIDowngradeRateLimitedRetryInterval / 2,
			maxDelay: openAIDowngradeRateLimitedRetryInterval * 3 / 2},
		{name: "storm_429", successes: 1, wantSuccesses: 1,
			streak:   openAIDowngrade429StreakThreshold - 1,
			result:   OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests},
			minDelay: openAIDowngrade429StreakBackoff,
			maxDelay: openAIDowngrade429StreakBackoff * 5 / 4},
		{name: "quota_429", successes: 1, wantSuccesses: 1,
			result: OpenAIDowngradeProbeResult{
				HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &resetAt,
				RateLimitWindow: "7d_window",
			},
			minDelay: resetAt.Sub(now),
			maxDelay: resetAt.Sub(now) + openAIDowngradeRateLimitResetStagger*3/2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID, UpdatedAt: now,
				Extra: map[string]any{OpenAIDowngradeSolFallbackExtraKey: true},
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
				require.Equal(t, "sol_fallback_astra", mode)
				result := tc.result
				result.AccountID, result.ProxyID = account.ID, &proxyID
				return result
			}
			state := OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
				AstraNextProbeAt: timePtr(now), AstraConsecutiveSuccesses: tc.successes,
				Consecutive429s: tc.streak, NextProbeAt: now.Add(-time.Minute), UpdatedAt: now,
			}
			require.NoError(t, runner.processStateAtomic(context.Background(), &state, now))
			require.Equal(t, 1, store.commits)
			require.NotNil(t, store.state)
			require.Equal(t, OpenAIDowngradeStateOnDuty, state.State)
			require.Equal(t, tc.wantSuccesses, state.AstraConsecutiveSuccesses)
			require.Equal(t, tc.wantFailures, state.AstraConsecutiveFailures)
			require.Equal(t, &now, state.LastProbeAt)
			minDelay, maxDelay := tc.minDelay, tc.maxDelay
			if minDelay == 0 {
				minDelay = openAIDowngradeDefaultInterval / 2
				maxDelay = openAIDowngradeDefaultInterval * 3 / 2
			}
			require.False(t, state.NextProbeAt.Before(now.Add(minDelay)))
			require.False(t, state.NextProbeAt.After(now.Add(maxDelay)))
			if tc.wantNormal {
				require.Equal(t, "normal", state.ProbeMode)
				require.Nil(t, state.AstraNextProbeAt)
				require.NotNil(t, store.observed.FallbackMode)
				require.False(t, *store.observed.FallbackMode)
			} else {
				require.Equal(t, "sol_fallback", state.ProbeMode)
				require.NotNil(t, state.AstraNextProbeAt)
				require.Nil(t, store.observed.FallbackMode)
				if tc.result.HTTPStatus == http.StatusTooManyRequests {
					require.Equal(t, now, *state.AstraNextProbeAt)
				} else {
					require.False(t, state.AstraNextProbeAt.Before(now.Add(openAIDowngradeSolFallbackInterval/2)))
					require.False(t, state.AstraNextProbeAt.After(now.Add(openAIDowngradeSolFallbackInterval*3/2)))
				}
			}
			require.Empty(t, repo.schedulableCalls)
			require.Empty(t, store.proxyChanges)
			require.Len(t, store.observed.Results, 1)
			require.Equal(t, &state, store.state)
			encoded, err := json.Marshal(store.state)
			require.NoError(t, err)
			var reloaded OpenAIDowngradeProbeState
			require.NoError(t, json.Unmarshal(encoded, &reloaded))
			require.Equal(t, state.NextProbeAt, reloaded.NextProbeAt)
			require.Equal(t, state.AstraNextProbeAt, reloaded.AstraNextProbeAt)
			require.Equal(t, state.ProbeMode, reloaded.ProbeMode)
		})
	}
}

func TestOpenAIDowngradeSolFallbackAstraScheduleCommitFailureDoesNotPublish(t *testing.T) {
	for _, commitErr := range []error{ErrOpenAIProbeStale, errors.New("outbox unavailable")} {
		t.Run(errorTestName(commitErr), func(t *testing.T) {
			now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
			proxyID := int64(3)
			account := &Account{
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID, UpdatedAt: now,
			}
			repo := &downgradeProbeAccountRepoStub{account: account}
			store := &downgradeAtomicStoreStub{
				downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo, commitErr: commitErr,
			}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{
					AccountID: account.ID, ProxyID: &proxyID, HTTPStatus: http.StatusOK,
					TransportOK: true, AnswerCorrect: true,
					ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
				}
			}
			state := OpenAIDowngradeProbeState{
				AccountID: account.ID, State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID, UpdatedAt: now,
				NextProbeAt: now.Add(-time.Minute), AstraNextProbeAt: timePtr(now),
				AstraConsecutiveSuccesses: 1,
			}
			before := state
			require.ErrorIs(t, runner.processStateAtomic(context.Background(), &state, now), commitErr)
			require.Equal(t, before, state)
			require.Equal(t, 1, store.commits)
			require.Nil(t, store.state)
			require.Empty(t, repo.fallbackModes)
			require.Empty(t, repo.schedulableCalls)
			require.Zero(t, repo.snapshotCalls)
			require.True(t, store.observed.State.NextProbeAt.After(now),
				"the candidate schedule must stay private when its commit fails")
		})
	}
}

func TestOpenAIDowngradeCandyQuestionKeepsAnchors(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		q := openAIDowngradeCandyQuestion(time.Time{})
		seen[q.Text] = struct{}{}
		// 行为锚点数字必须在场：目标1 7/7、目标2 9/6、干扰 8/4
		for _, anchor := range []string{"7", "9", "8", "6", "4"} {
			if !strings.Contains(q.Text, anchor) {
				t.Fatalf("question missing anchored number %s:\n%s", anchor, q.Text)
			}
		}
		if !strings.Contains(q.Text, "味") || !strings.Contains(q.Text, "最少取出多少个糖果") {
			t.Fatalf("question lost required structure:\n%s", q.Text)
		}
		if !q.AnswerPattern.MatchString("答案是21") || q.AnswerPattern.MatchString("答案是210") {
			t.Fatalf("candy answer pattern should match 21 with digit boundaries")
		}
	}
	if len(seen) < 50 {
		t.Fatalf("expected high question text diversity, got %d distinct in 200 draws", len(seen))
	}
}

func TestOpenAIDowngradeProbePausedAccountYieldsQueueHead(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	proxyID := int64(3)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &proxyID,
	}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("manually paused account must not be probed")
		return OpenAIDowngradeProbeResult{}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
		NextProbeAt: now.Add(-time.Hour), // overdue, would pin the per-IP queue head
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Zero(t, store.probeCalls)
	require.Equal(t, 1, store.saveCalls)
	// spread 正向散布：回访时刻不早于 30 分钟下界，也不越过 +25% 上界。
	minNext := now.Add(openAIDowngradeHalfOpenInterval)
	maxNext := now.Add(time.Duration(float64(openAIDowngradeHalfOpenInterval) * 1.25))
	require.True(t, !state.NextProbeAt.Before(minNext) && !state.NextProbeAt.After(maxNext),
		"paused-account revisit %v outside spread window [%v, %v]",
		state.NextProbeAt, minNext, maxNext)
}

func TestProbeZombieStateDeletedWhenAccountGone(t *testing.T) {
	// 2026-09-17 根因回归：面板软删账号的 state 若只报错不删，ListDue 每
	// IP 桶 rank-1 被僵尸钉死，同桶活号零探针（1054 钉死 p5 → 1055 降智
	// 5.5h 未检出，15 僵尸钉死全部 5 个在用桶）。processState 收到
	// ErrAccountNotFound 哨兵必须就地删除；删除失败退化为让位重排；
	// 瞬时 DB 错误（非哨兵）不删不重排原样上抛。
	now := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{getByIDErr: ErrAccountNotFound}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("gone account must not be probed")
		return OpenAIDowngradeProbeResult{}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1054, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		NextProbeAt: now.Add(-time.Hour), // overdue → would pin the queue head
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, []int64{1054}, store.deleteStateCalls, "zombie state must be deleted inline")
	require.Zero(t, store.probeCalls)
	require.Zero(t, store.saveCalls)

	// 删除失败（暂态 DB 故障）：退化为让位重排，绝不留在队首。
	store2 := &downgradeProbeStoreStub{deleteStateErr: errors.New("delete failed")}
	repo2 := &downgradeProbeAccountRepoStub{getByIDErr: ErrAccountNotFound}
	runner2 := NewOpenAIDowngradeProbeRunner(store2, repo2, nil, nil, nil, nil)
	runner2.now = func() time.Time { return now }
	state2 := &OpenAIDowngradeProbeState{AccountID: 1054, NextProbeAt: now.Add(-time.Hour)}
	require.NoError(t, runner2.processState(context.Background(), state2, now))
	require.Equal(t, 1, store2.saveCalls)
	minNext := now.Add(openAIDowngradeHalfOpenInterval)
	maxNext := now.Add(time.Duration(float64(openAIDowngradeHalfOpenInterval) * 1.25))
	require.True(t, !state2.NextProbeAt.Before(minNext) && !state2.NextProbeAt.After(maxNext),
		"fallback reschedule %v outside spread window [%v, %v]", state2.NextProbeAt, minNext, maxNext)

	// 瞬时 DB 错误（非 NotFound 哨兵）：不删不重排，原样上抛等下轮重试。
	store3 := &downgradeProbeStoreStub{}
	repo3 := &downgradeProbeAccountRepoStub{getByIDErr: errors.New("connection reset")}
	runner3 := NewOpenAIDowngradeProbeRunner(store3, repo3, nil, nil, nil, nil)
	state3 := &OpenAIDowngradeProbeState{AccountID: 1054, NextProbeAt: now.Add(-time.Hour)}
	require.Error(t, runner3.processState(context.Background(), state3, now))
	require.Empty(t, store3.deleteStateCalls)
	require.Zero(t, store3.saveCalls)
}

func TestProbeIneligibleLiveAccountYieldsQueueHead(t *testing.T) {
	// 资格（平台/类型/影子/过期）与 status 两条路径原为 return nil 静默跳过：
	// state 不重排 → 下一轮仍 due → 同桶 rank-1 永久钉死。现在必须镜像
	// 手动暂停分支：不探 + 排期后移让位（30-37.5min spread）。
	for _, tc := range []struct {
		name   string
		mutate func(*Account)
	}{
		{"other_platform", func(a *Account) { a.Platform = "anthropic" }},
		{"non_oauth", func(a *Account) { a.Type = "apikey" }},
		{"expired", func(a *Account) {
			a.ExpiresAt = timePtr(nowAdd(time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC), -time.Minute))
			a.AutoPauseOnExpired = true
		}},
		{"disabled", func(a *Account) { a.Status = "disabled" }},
		{"inactive", func(a *Account) { a.Status = "inactive" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
			proxyID := int64(5)
			account := &Account{
				ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID,
			}
			tc.mutate(account)
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{account: account}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				t.Fatal("ineligible account must not be probed")
				return OpenAIDowngradeProbeResult{}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				OriginalProxyID: &proxyID, CurrentProxyID: &proxyID,
				NextProbeAt: now.Add(-time.Hour),
			}

			require.NoError(t, runner.processState(context.Background(), state, now))
			require.Zero(t, store.probeCalls, "must not probe")
			require.Equal(t, 1, store.saveCalls, "must reschedule to yield the queue head")
			require.Empty(t, store.deleteStateCalls, "live account state must not be deleted")
			minNext := now.Add(openAIDowngradeHalfOpenInterval)
			maxNext := now.Add(time.Duration(float64(openAIDowngradeHalfOpenInterval) * 1.25))
			require.True(t, !state.NextProbeAt.Before(minNext) && !state.NextProbeAt.After(maxNext),
				"yield reschedule %v outside spread window [%v, %v]", state.NextProbeAt, minNext, maxNext)
		})
	}
}

func TestOpenAIDowngradeProbeDirectAccountDoesNotArmQualification(t *testing.T) {
	now := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	// Direct imports stay schedulable while normal health checks are queued.
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, ProxyID: nil,
	}
	repo := &downgradeProbeAccountRepoStub{account: account}
	store := &downgradeAtomicStoreStub{downgradeProbeStoreStub: &downgradeProbeStoreStub{}, accountRepo: repo}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		t.Fatal("arm pass must not probe")
		return OpenAIDowngradeProbeResult{}
	}

	require.NoError(t, runner.RunOnce(context.Background()))
	require.Empty(t, repo.schedulableCalls)
	require.True(t, account.Schedulable)
	require.NotNil(t, store.state)
	require.NotEqual(t, "qualification", store.state.ProbeMode)
	require.True(t, store.state.NextProbeAt.After(now))
}

func TestOpenAIDowngradeProbeQualificationUsesAcceleratedCadence(t *testing.T) {
	mainProxyID := int64(8)
	now := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: false, ProxyID: &mainProxyID,
	}
	store := &downgradeProbeStoreStub{mainProxyID: &mainProxyID}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	// nextDelay 返回超大值：若资格路径误用它，断言窗口必然失败。
	runner.nextDelay = func() time.Duration { return 75 * time.Minute }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty,
		ProbeMode: "qualification", NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.True(t, account.Schedulable, "r15h: single pass promotes immediately")
	require.Equal(t, "normal", state.ProbeMode,
		"first pass promotes; no second certification needle needed")
	require.Equal(t, 0, state.ConsecutiveSuccesses, "promotion resets the streak")
	// 解锁后的首针 on-duty 复查仍按分钟级排（jitter 后 2.5-7.5 分钟），
	// 随后回归 15-75 分钟常规随机节奏，不落固定间隔。
	lo := now.Add(time.Duration(float64(openAIDowngradeQualificationInterval) * 0.5))
	hi := now.Add(time.Duration(float64(openAIDowngradeQualificationInterval) * 1.5))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"qualification cadence %v outside accelerated window [%v, %v]",
		state.NextProbeAt, lo, hi)
}

func TestProbe429BodyClassificationDistinguishesExhaustion(t *testing.T) {
	// 机制必须能检测到额度耗尽（2026-09-15 用户裁定）：usage_limit_reached 带
	// resets_at/resets_in_seconds 的体能解析出重置时间；未知类型的普通限流体
	// 解析不出，继续走短周期重探。
	exhausted := []byte(`{"error":{"message":"The usage limit has been reached","type":"usage_limit_reached","resets_at":1790000000}}`)
	require.NotNil(t, parseOpenAIRateLimitResetTime(exhausted))
	require.Equal(t, int64(1790000000), *parseOpenAIRateLimitResetTime(exhausted))

	inSeconds := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
	ts := parseOpenAIRateLimitResetTime(inSeconds)
	require.NotNil(t, ts)
	require.InDelta(t, time.Now().Add(time.Hour).Unix(), *ts, 5)

	generic := []byte(`{"error":{"message":"Too many requests","type":"rate_limited"}}`)
	require.Nil(t, parseOpenAIRateLimitResetTime(generic))
}

func TestProbe429WithResetTimeDefersToResetPoint(t *testing.T) {
	// 额度耗尽类 429：下一针排在「重置点+错峰(15-45min)」窗内，期间零探针；
	// 计数器不动（限流不是降智证据），事件落 rate_limit_deferred。
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	resetAt := now.Add(2 * time.Hour)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &resetAt,
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		ConsecutiveSuccesses: 2, NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls)
	lo := resetAt.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 0.5))
	hi := resetAt.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 1.5))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"reset backoff %v outside stagger window [%v, %v]", state.NextProbeAt, lo, hi)
	require.Equal(t, 2, state.ConsecutiveSuccesses, "deferral must not touch counters")
	require.Equal(t, 0, state.ConsecutiveFailures)
	require.Equal(t, 1, store.eventCalls)
	require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets, "an unknown window still imposes a deadline")
	require.Equal(t, "unknown_window", store.eventDetails[0]["class"])
}

func TestProbe429ResetTimeFloorWithoutEarlyCap(t *testing.T) {
	newFixture := func(resetAt time.Time) (*OpenAIDowngradeProbeRunner, *OpenAIDowngradeProbeState, time.Time) {
		now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, Schedulable: true}
		runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{},
			&downgradeProbeAccountRepoStub{account: account}, nil, nil, nil, nil)
		runner.now = func() time.Time { return now }
		runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
			return OpenAIDowngradeProbeResult{
				HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &resetAt,
			}
		}
		state := &OpenAIDowngradeProbeState{
			AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
			NextProbeAt: now,
		}
		return runner, state, now
	}

	// 重置点已过/过近：floor 到 now+30min 再叠错峰。
	runner, state, now := newFixture(nowAdd(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC), -time.Hour))
	require.NoError(t, runner.processState(context.Background(), state, now))
	floor := now.Add(openAIDowngradeRateLimitResetFloor)
	lo := floor.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 0.5))
	hi := floor.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 1.5))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"floored backoff %v outside window [%v, %v]", state.NextProbeAt, lo, hi)

	// A valid distant deadline must not be truncated into an early retry.
	runner, state, now = newFixture(nowAdd(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC), 90*24*time.Hour))
	require.NoError(t, runner.processState(context.Background(), state, now))
	lo = now.Add(90*24*time.Hour + openAIDowngradeRateLimitResetStagger/2)
	hi = now.Add(90*24*time.Hour + openAIDowngradeRateLimitResetStagger*3/2)
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"backoff %v outside deadline window [%v, %v]", state.NextProbeAt, lo, hi)
}

func nowAdd(base time.Time, d time.Duration) time.Time { return base.Add(d) }

func TestProbe429StormEscalatesAndClearsOnSuccess(t *testing.T) {
	// 无重置时间的 429：前 5 针维持短周期重探，第 6 针起风暴退避 1 小时；
	// 中途任何非 429 结果清零计数，回到短周期语义。
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return result
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
		NextProbeAt: now,
	}
	shortLo := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 0.5))
	shortHi := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 1.5))

	for i := 0; i < openAIDowngrade429StreakThreshold-1; i++ {
		require.NoError(t, runner.processState(context.Background(), state, now))
		require.True(t, !state.NextProbeAt.Before(shortLo) && !state.NextProbeAt.After(shortHi),
			"pre-threshold 429 #%d must stay short-cycle, got %v", i+1, state.NextProbeAt)
	}
	require.Zero(t, store.eventCalls, "no deferral event before threshold")

	require.NoError(t, runner.processState(context.Background(), state, now))
	stormLo := now.Add(openAIDowngrade429StreakBackoff)
	stormHi := now.Add(time.Duration(float64(openAIDowngrade429StreakBackoff) * 1.25))
	require.True(t, !state.NextProbeAt.Before(stormLo) && !state.NextProbeAt.After(stormHi),
		"storm backoff %v outside spread window [%v, %v]", state.NextProbeAt, stormLo, stormHi)
	require.Equal(t, 1, store.eventCalls)

	// 一针健康结果清零计数，随后的 429 回到短周期。
	result = OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true,
		ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum)}
	require.NoError(t, runner.processState(context.Background(), state, now))
	result = OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}
	require.NoError(t, runner.processState(context.Background(), state, now))
	require.True(t, !state.NextProbeAt.Before(shortLo) && !state.NextProbeAt.After(shortHi),
		"429 after streak reset must return to short-cycle, got %v", state.NextProbeAt)
	require.Equal(t, 1, store.eventCalls, "no new storm event after reset")
}

func TestProbe429LongHoldNeverGetsEarlyDailyRecheck(t *testing.T) {
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(7 * 24 * time.Hour)
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
	handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
		OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
			RateLimitResetAt: &resetAt, RateLimitWindow: "7d_window"}, now)
	require.True(t, handled)
	require.NoError(t, err)
	recheckLo := resetAt.Add(openAIDowngradeRateLimitResetStagger / 2)
	recheckHi := resetAt.Add(openAIDowngradeRateLimitResetStagger * 3 / 2)
	require.True(t, !state.NextProbeAt.Before(recheckLo) && !state.NextProbeAt.After(recheckHi),
		"long-hold recheck %v outside reset window [%v, %v]", state.NextProbeAt, recheckLo, recheckHi)
	require.NotEqual(t, true, store.eventDetails[0]["recheck"])
	require.Len(t, repo.rateLimitedResets, 1, "7d window must still extend the account hold")
}

func TestProbe429ShortWindowCadenceUnchangedByRecheck(t *testing.T) {
	// Short windows retain their existing post-deadline probe stagger.
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	store := &downgradeProbeStoreStub{}
	runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
	handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
		OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
			RateLimitResetAt: &resetAt, RateLimitWindow: "5h_window"}, now)
	require.True(t, handled)
	require.NoError(t, err)
	lo := resetAt.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 0.5))
	hi := resetAt.Add(time.Duration(float64(openAIDowngradeRateLimitResetStagger) * 1.5))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"short-window backoff %v outside stagger window [%v, %v]", state.NextProbeAt, lo, hi)
	require.NotEqual(t, true, store.eventDetails[0]["recheck"], "short window must not be recheck-scheduled")
	require.Equal(t, "5h_window", store.eventDetails[0]["class"])
}

func TestProbe429RecheckConvergesTowardResetPoint(t *testing.T) {
	// A reset near the old daily recheck boundary still forbids early probes.
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(23 * time.Hour)
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
	handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
		OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
			RateLimitResetAt: &resetAt, RateLimitWindow: "unknown_window"}, now)
	require.True(t, handled)
	require.NoError(t, err)
	lo := resetAt.Add(openAIDowngradeRateLimitResetStagger / 2)
	hi := resetAt.Add(openAIDowngradeRateLimitResetStagger * 3 / 2)
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"converged backoff %v outside envelope [%v, %v]", state.NextProbeAt, lo, hi)
}

func TestProbe429UnknownWindowFarResetHoldsAccount(t *testing.T) {
	now := time.Date(2026, 9, 18, 17, 51, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		window string
		delay  time.Duration
		hold   bool
	}{
		{"unknown_distant", "unknown_window", 30 * 24 * time.Hour, true},
		{"unknown_far", "unknown_window", 6 * 24 * time.Hour, true},
		{"unknown_near", "unknown_window", 2 * time.Hour, false},
		{"unknown_at_boundary", "unknown_window", openAIDowngradeRateLimitQuotaLikeDistance, false},
		{"unknown_over_boundary", "unknown_window", openAIDowngradeRateLimitQuotaLikeDistance + time.Nanosecond, true},
		{"short_window", "5h_window", 4 * time.Hour, false},
		{"short_window_far", "5h_window", 6 * 24 * time.Hour, false},
		{"weekly_near", "7d_window", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			resetAt := now.Add(tc.delay)
			state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
			handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
				OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
					RateLimitResetAt: &resetAt, RateLimitWindow: tc.window}, now)
			require.True(t, handled)
			require.NoError(t, err)
			require.Len(t, store.eventDetails, 1)
			require.Equal(t, tc.hold, store.eventDetails[0]["quota_like"])
			require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets)
			require.NotEqual(t, true, store.eventDetails[0]["capped"])
		})
	}
}

func TestProbe429WhileHeldSkipsStormGate(t *testing.T) {
	// 账号已在限流持有中（reset 未到）再吃无时间信息 429：不进 1 小时风暴闸、
	// 不计数（限流非降智证据），锚定持有截止时间。未持有账号保持原语义。
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	limitedAt := now.Add(-time.Hour)
	heldUntil := now.Add(14 * 24 * time.Hour)
	result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}

	store := &downgradeProbeStoreStub{}
	held := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		RateLimitedAt: &limitedAt, RateLimitResetAt: &heldUntil}
	runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{account: held}, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
	handled, err := runner.applyRateLimitDeferral(context.Background(), held, state, result, now)
	require.True(t, handled)
	require.NoError(t, err)
	lo := heldUntil.Add(openAIDowngradeRateLimitResetStagger / 2)
	hi := heldUntil.Add(openAIDowngradeRateLimitResetStagger * 3 / 2)
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"held-account no-info 429 must wait until reset, got %v", state.NextProbeAt)
	require.Zero(t, state.Consecutive429s, "held no-info 429 must not feed the storm counter")
	require.Equal(t, "recheck_streak_suppressed", store.eventDetails[0]["class"])

	// 对照组：无持有的同款 429 仍走短周期+计数（r15g 语义不变）。
	free := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	store2 := &downgradeProbeStoreStub{}
	runner2 := NewOpenAIDowngradeProbeRunner(store2, &downgradeProbeAccountRepoStub{account: free}, nil, nil, nil, nil)
	state2 := &OpenAIDowngradeProbeState{AccountID: 2, NextProbeAt: now}
	handled2, err2 := runner2.applyRateLimitDeferral(context.Background(), free, state2, result, now)
	require.True(t, handled2)
	require.NoError(t, err2)
	shortLo := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 0.5))
	shortHi := now.Add(time.Duration(float64(openAIDowngradeRateLimitedRetryInterval) * 1.5))
	require.True(t, !state2.NextProbeAt.Before(shortLo) && !state2.NextProbeAt.After(shortHi),
		"unheld no-info 429 must stay short-cycle, got %v", state2.NextProbeAt)
	require.Equal(t, 1, state2.Consecutive429s)
}

func TestProbe429WhileHeldPreservesNearReset(t *testing.T) {
	now := time.Date(2026, 9, 18, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		delay time.Duration
		held  bool
	}{
		{"near_reset", time.Hour, true},
		{"at_reset", 0, false},
		{"expired_reset", -time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAt := now.Add(tc.delay)
			account := &Account{ID: 1, RateLimitResetAt: &resetAt}
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, NextProbeAt: now,
				ConsecutiveSuccesses: 2, ConsecutiveFailures: 1,
			}
			handled, err := runner.applyRateLimitDeferral(context.Background(), account, state,
				OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}, now)
			require.True(t, handled)
			require.NoError(t, err)
			if tc.held {
				lo := resetAt.Add(openAIDowngradeRateLimitResetStagger / 2)
				hi := resetAt.Add(openAIDowngradeRateLimitResetStagger * 3 / 2)
				require.False(t, state.NextProbeAt.Before(lo))
				require.False(t, state.NextProbeAt.After(hi), "a known near reset must not wait for the daily recheck")
				require.Zero(t, state.Consecutive429s)
				require.Len(t, store.eventDetails, 1)
				require.Equal(t, "recheck_streak_suppressed", store.eventDetails[0]["class"])
			} else {
				lo := now.Add(openAIDowngradeRateLimitedRetryInterval / 2)
				hi := now.Add(openAIDowngradeRateLimitedRetryInterval * 3 / 2)
				require.False(t, state.NextProbeAt.Before(lo))
				require.False(t, state.NextProbeAt.After(hi))
				require.Equal(t, 1, state.Consecutive429s)
				require.Empty(t, store.eventDetails)
			}
			require.Equal(t, 2, state.ConsecutiveSuccesses)
			require.Equal(t, 1, state.ConsecutiveFailures)
			require.Equal(t, 1, store.saveCalls)
			if tc.held {
				require.Empty(t, repo.rateLimitedResets)
			} else {
				require.Equal(t, []time.Time{state.NextProbeAt}, repo.rateLimitedResets)
			}
			require.Empty(t, repo.openAIRateLimitClears)
			require.Equal(t, now.Add(tc.delay), *account.RateLimitResetAt)
		})
	}
}

func TestProbeAcceptedResultReleasesHeldRateLimit(t *testing.T) {
	// 复查针拿到上游真实接受（传输 OK 且 2xx）：按 CAS 清除观察到的持有对，
	// 落 rate_limit_recheck_recovered 事件；handled=false 让成功针照常走状态机。
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	limitedAt := now.Add(-time.Hour)
	heldUntil := now.Add(14 * 24 * time.Hour)
	makeFixture := func() (*OpenAIDowngradeProbeRunner, *downgradeProbeStoreStub, *downgradeProbeAccountRepoStub, *Account, *OpenAIDowngradeProbeState) {
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			RateLimitedAt: &limitedAt, RateLimitResetAt: &heldUntil}
		store := &downgradeProbeStoreStub{}
		repo := &downgradeProbeAccountRepoStub{account: account, openAIRateLimitClearResult: true}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
		return runner, store, repo, account, state
	}

	runner, store, repo, account, state := makeFixture()
	handled, err := runner.applyRateLimitDeferral(context.Background(), account, state,
		OpenAIDowngradeProbeResult{TransportOK: true, HTTPStatus: http.StatusOK}, now)
	require.False(t, handled, "accepted result must continue into the state machine")
	require.NoError(t, err)
	require.Len(t, repo.openAIRateLimitClears, 1, "accepted probe must CAS-clear the observed hold")
	require.Equal(t, int64(1), repo.openAIRateLimitClears[0].id)
	require.Equal(t, limitedAt, repo.openAIRateLimitClears[0].limitedAt)
	require.Equal(t, heldUntil, repo.openAIRateLimitClears[0].resetAt)
	require.Equal(t, OpenAIDowngradeEventRateLimitRecheckRecovered, store.eventTypes[0])

	// 非 2xx（如 401 token 失效）不是「上游接受」，不得释放持有。
	runner2, store2, repo2, account2, state2 := makeFixture()
	handled2, err2 := runner2.applyRateLimitDeferral(context.Background(), account2, state2,
		OpenAIDowngradeProbeResult{TransportOK: true, HTTPStatus: http.StatusUnauthorized}, now)
	require.False(t, handled2)
	require.NoError(t, err2)
	require.Empty(t, repo2.openAIRateLimitClears, "non-2xx must not release the hold")
	require.Zero(t, store2.eventCalls)

	// CAS 未命中（他处已延长/改写持有）：不清、不落恢复事件。
	runner3, store3, repo3, account3, state3 := makeFixture()
	repo3.openAIRateLimitClearResult = false
	handled3, err3 := runner3.applyRateLimitDeferral(context.Background(), account3, state3,
		OpenAIDowngradeProbeResult{TransportOK: true, HTTPStatus: http.StatusOK}, now)
	require.False(t, handled3)
	require.NoError(t, err3)
	require.Len(t, repo3.openAIRateLimitClears, 1, "CAS attempt still happens")
	require.Zero(t, store3.eventCalls, "unmatched CAS must not emit recovery event")
}

func TestProbeOpenAI429ResetTimeParsesCodexHeaders(t *testing.T) {
	// x-codex-* 窗口头优先于 body：secondary(5h 窗)与 primary(7d 窗)都能解析出
	// 重置时间；头缺时回退 body；两处都没有时间信息返回 nil（走短周期/风暴闸）。
	headers5h := http.Header{}
	headers5h.Set("x-codex-secondary-used-percent", "100")
	headers5h.Set("x-codex-secondary-reset-after-seconds", "3600")
	resetAt := probeOpenAI429ResetTime(headers5h, nil)
	require.NotNil(t, resetAt)
	require.InDelta(t, time.Now().Add(time.Hour).Unix(), resetAt.Unix(), 5)

	headers7d := http.Header{}
	headers7d.Set("x-codex-primary-used-percent", "100")
	headers7d.Set("x-codex-primary-reset-after-seconds", "259200")
	resetAt = probeOpenAI429ResetTime(headers7d, nil)
	require.NotNil(t, resetAt)
	require.InDelta(t, time.Now().Add(72*time.Hour).Unix(), resetAt.Unix(), 5)

	futureReset := time.Now().Add(48 * time.Hour).Unix()
	body := []byte(fmt.Sprintf(`{"error":{"type":"usage_limit_reached","resets_at":%d}}`, futureReset))
	resetAt = probeOpenAI429ResetTime(http.Header{}, body)
	require.NotNil(t, resetAt)
	require.Equal(t, futureReset, resetAt.Unix())

	require.Nil(t, probeOpenAI429ResetTime(http.Header{}, []byte(`{"error":{"type":"rate_limited"}}`)))
}

func TestProbe429WindowClassificationRequiresExplicitEvidence(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		window string
		delay  time.Duration
		hold   bool
	}{
		{"short", "5h_window", 2 * time.Hour, false},
		{"week", "7d_window", 72 * time.Hour, true},
		{"week_near_reset", "7d_window", time.Hour, true},
		{"unknown_near", "", time.Hour, false},
		{"unknown_far", "", 72 * time.Hour, true},
		{"invalid_label", "weekly", 72 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAt := now.Add(tc.delay)
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true}
			store := &downgradeProbeStoreStub{}
			repo := &downgradeProbeAccountRepoStub{account: account}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.now = func() time.Time { return now }
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{
					HTTPStatus: http.StatusTooManyRequests, RateLimitResetAt: &resetAt,
					RateLimitWindow: tc.window,
				}
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal",
				NextProbeAt: now,
			}
			require.NoError(t, runner.processState(context.Background(), state, now))
			require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets)
			require.Equal(t, tc.hold, store.eventDetails[0]["quota_like"])
			require.True(t, state.NextProbeAt.After(resetAt))
			expected := tc.window
			if expected != "5h_window" && expected != "7d_window" {
				expected = "unknown_window"
			}
			require.Equal(t, expected, store.eventDetails[0]["class"])
		})
	}
}

func TestProbe429HeaderWindowIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, primaryWindow, secondaryWindow, primaryUsed, secondaryUsed, primaryReset, secondaryReset, want string
	}{
		{"weekly_near_reset", "10080", "300", "100", "10", "600", "18000", "7d_window"},
		{"weekly_swapped", "300", "10080", "10", "100", "18000", "600", "7d_window"},
		{"short", "10080", "300", "10", "100", "600", "3600", "5h_window"},
		{"short_swapped", "300", "10080", "100", "10", "3600", "600", "5h_window"},
		{"missing_windows", "", "", "100", "10", "600", "3600", "unknown_window"},
		{"neither_exhausted", "10080", "300", "10", "10", "600", "3600", "unknown_window"},
		{"unknown_exhausted_long", "", "300", "100", "100", "600", "3600", "unknown_window"},
		{"missing_weekly_reset", "10080", "300", "100", "100", "", "3600", "unknown_window"},
		{"negative_reset", "10080", "300", "100", "10", "-1", "3600", "unknown_window"},
		{"duplicate_windows", "10080", "10080", "100", "100", "600", "3600", "unknown_window"},
		{"both_exhausted", "10080", "300", "100", "100", "600", "3600", "7d_window"},
		{"nonstandard_window", "1440", "300", "100", "10", "600", "3600", "unknown_window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			headers.Set("x-codex-primary-window-minutes", tc.primaryWindow)
			headers.Set("x-codex-secondary-window-minutes", tc.secondaryWindow)
			headers.Set("x-codex-primary-used-percent", tc.primaryUsed)
			headers.Set("x-codex-secondary-used-percent", tc.secondaryUsed)
			headers.Set("x-codex-primary-reset-after-seconds", tc.primaryReset)
			headers.Set("x-codex-secondary-reset-after-seconds", tc.secondaryReset)
			require.Equal(t, tc.want, probeOpenAI429Window(headers))
		})
	}
	require.Equal(t, "unknown_window", probeOpenAI429Window(nil))
}

func TestProbeFirstSolFallback429DefersWithoutReplacement(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, withReset := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic", true: "explicit_reset"}[withReset], func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			// r17aq：runProbeRecorded 在针后 GetByID 复读账号做新鲜度守卫，
			// stub 必须回同一个账号，否则结果被当 stale 丢弃。
			solAccount := &Account{ID: 1}
			repo := &downgradeProbeAccountRepoStub{account: solAccount}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			resetAt := now.Add(time.Hour)
			runner.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
				require.Equal(t, "sol_fallback", mode)
				result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}
				if withReset {
					result.RateLimitResetAt = &resetAt
					result.RateLimitWindow = "7d_window"
				}
				return result
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateReprobe, ProbeMode: "normal",
				ConsecutiveFailures: 2, ConsecutiveSuccesses: 1, NextProbeAt: now,
			}
			require.NoError(t, runner.startSolFallback(context.Background(), solAccount, state, now))
			require.Equal(t, OpenAIDowngradeStateReprobe, state.State)
			require.Equal(t, "sol_fallback", state.ProbeMode)
			require.Equal(t, 2, state.ConsecutiveFailures)
			require.Equal(t, 1, state.ConsecutiveSuccesses)
			require.True(t, state.NextProbeAt.After(now))
			require.Empty(t, store.proxyChanges)
			require.Empty(t, repo.schedulableCalls)
			require.Empty(t, repo.fallbackModes)
			require.Equal(t, 1, store.saveCalls)
			// Simulate the next scan from persisted state, not a direct Sol call.
			repo.account = &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive}
			require.NoError(t, runner.processState(context.Background(), store.state, state.NextProbeAt))
			require.Equal(t, "sol_fallback", store.state.ProbeMode)
			require.Equal(t, 2, store.probeCalls)
		})
	}
}

func TestProbeFirstSolFallbackInconclusiveDoesNotReplace(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		result OpenAIDowngradeProbeResult
	}{
		{"transport", OpenAIDowngradeProbeResult{}},
		{"upstream_503", OpenAIDowngradeProbeResult{HTTPStatus: http.StatusServiceUnavailable}},
		{"missing_usage", OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true}},
		{"middle_band", OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1100)}},
		{"correct_fingerprint", OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true, ReasoningTokens: downgradeProbeIntPtr(1552)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			// 同上：新鲜度守卫要求 GetByID 能回同一账号。
			solAccount := &Account{ID: 1}
			repo := &downgradeProbeAccountRepoStub{account: solAccount}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return tc.result
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateReprobe,
				ConsecutiveFailures: 2, NextProbeAt: now,
			}
			require.NoError(t, runner.startSolFallback(context.Background(), solAccount, state, now))
			require.Equal(t, OpenAIDowngradeStateReprobe, state.State)
			require.Equal(t, "sol_fallback", state.ProbeMode)
			require.False(t, state.NextProbeAt.Before(now.Add(openAIDowngradeProbeProxyMinInterval)))
			require.Empty(t, store.proxyChanges)
			require.Empty(t, repo.schedulableCalls)
			require.Empty(t, repo.fallbackModes)
			require.Zero(t, store.eventCalls)
		})
	}
}

func TestProbe429AllTracksUseSharedDeferral(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, initial := range []OpenAIDowngradeProbeState{
		{State: OpenAIDowngradeStateOnDuty, ProbeMode: "normal"},
		{State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification"},
		{State: OpenAIDowngradeStateReprobe, ProbeMode: "normal"},
		{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "half_open"},
		{State: OpenAIDowngradeStateReprobe, ProbeMode: "sol_fallback"},
		{State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback"},
		{State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback", AstraNextProbeAt: &now},
	} {
		for _, window := range []string{"", "5h_window", "7d_window", "unknown_window"} {
			t.Run(initial.State+"/"+initial.ProbeMode+"/"+window, func(t *testing.T) {
				proxyID := int64(3)
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ProxyID: &proxyID}
				store := &downgradeProbeStoreStub{}
				repo := &downgradeProbeAccountRepoStub{account: account}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				resetAt := now.Add(time.Hour)
				if window == "unknown_window" {
					resetAt = now.Add(72 * time.Hour)
				}
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests, RateLimitWindow: window}
					if window != "" {
						result.RateLimitResetAt = &resetAt
					}
					return result
				}
				state := initial
				state.AccountID = 1
				state.CurrentProxyID = &proxyID
				state.OriginalProxyID = &proxyID
				state.ConsecutiveFailures = 1
				state.ConsecutiveSuccesses = 1
				require.NoError(t, runner.processState(context.Background(), &state, now))
				require.Equal(t, initial.State, state.State)
				require.Equal(t, initial.ProbeMode, state.ProbeMode)
				require.Equal(t, 1, state.ConsecutiveFailures)
				require.Equal(t, 1, state.ConsecutiveSuccesses)
				require.Equal(t, 1, store.probeCalls)
				require.Equal(t, 1, store.saveCalls)
				require.Empty(t, store.proxyChanges)
				require.Empty(t, repo.schedulableCalls)
				require.Empty(t, repo.fallbackModes)
				if window != "" {
					require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets)
				} else {
					require.Equal(t, []time.Time{state.NextProbeAt}, repo.rateLimitedResets)
				}
			})
		}
	}
}

func TestPendingSolFallbackSurvivesStateReload(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	proxyID := int64(3)
	for _, initialResult := range []OpenAIDowngradeProbeResult{
		{HTTPStatus: http.StatusTooManyRequests},
		{HTTPStatus: http.StatusServiceUnavailable},
		{TransportOK: false},
	} {
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Status: StatusActive, ProxyID: &proxyID}
		repo := &downgradeProbeAccountRepoStub{account: account}
		store := &downgradeProbeStoreStub{}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		runner.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
			require.Equal(t, "sol_fallback", mode)
			return initialResult
		}
		state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateReprobe,
			ProbeMode: "normal", CurrentProxyID: &proxyID, OriginalProxyID: &proxyID}
		require.NoError(t, runner.startSolFallback(context.Background(), account, state, now))
		require.False(t, account.Schedulable)
		require.Empty(t, repo.fallbackModes)
		require.NotNil(t, store.state)
		persisted, err := json.Marshal(store.state)
		require.NoError(t, err)
		var reloaded OpenAIDowngradeProbeState
		require.NoError(t, json.Unmarshal(persisted, &reloaded))

		restarted := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		restarted.probeFn = func(_ context.Context, _ *Account, mode string) OpenAIDowngradeProbeResult {
			require.Equal(t, "sol_fallback", mode, "retry must not fall back to the Astra reprobe track")
			return OpenAIDowngradeProbeResult{TransportOK: true, AnswerCorrect: true,
				HTTPStatus: http.StatusOK, ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum)}
		}
		require.NoError(t, restarted.processState(context.Background(), &reloaded, reloaded.NextProbeAt))
		require.Equal(t, OpenAIDowngradeStateOnDuty, reloaded.State)
		require.Equal(t, "sol_fallback", reloaded.ProbeMode)
		require.True(t, account.Schedulable)
		require.Equal(t, []bool{true}, repo.fallbackModes)
		require.Empty(t, store.proxyChanges)
	}
}

func TestProbe429CooldownFailureIsNotSuccess(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	writeErr := errors.New("cooldown write failed")
	for _, tc := range []struct {
		name string
		repo AccountRepository
	}{
		{"unavailable", nil},
		{"write_failed", &downgradeProbeAccountRepoStub{rateLimitedErr: writeErr}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, tc.repo, nil, nil, nil, nil)
			state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
			handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
				OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
					RateLimitResetAt: &resetAt, RateLimitWindow: "7d_window"}, now)
			require.True(t, handled)
			require.Error(t, err)
			if tc.name == "write_failed" {
				require.ErrorIs(t, err, writeErr)
			}
			require.Zero(t, store.saveCalls)
			require.Zero(t, store.eventCalls)
			require.Equal(t, now, state.NextProbeAt)
		})
	}
}

func TestProbe429PersistenceErrorsPropagate(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	writeErr := errors.New("persistence failed")
	for _, stage := range []string{"event", "state", "short_state"} {
		t.Run(stage, func(t *testing.T) {
			store := &downgradeProbeStoreStub{}
			result := OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests}
			if stage != "short_state" {
				result.RateLimitResetAt = timePtr(now.Add(time.Hour))
			}
			if stage == "event" {
				store.eventErr = writeErr
			} else {
				store.saveErr = writeErr
			}
			runner := NewOpenAIDowngradeProbeRunner(store, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
			handled, err := runner.applyRateLimitDeferral(context.Background(), nil,
				&OpenAIDowngradeProbeState{AccountID: 1}, result, now)
			require.True(t, handled)
			require.ErrorIs(t, err, writeErr)
		})
	}
}

func TestProbeDisabledAccountCannotEnterRescue(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, status := range []string{"disabled", "inactive", "", "unknown"} {
		for _, initial := range []OpenAIDowngradeProbeState{
			{State: OpenAIDowngradeStateOnDuty, ProbeMode: "qualification"},
			{State: OpenAIDowngradeStateCircuitOpen},
			{State: OpenAIDowngradeStateCircuitOpen, ProbeMode: "half_open"},
			{State: OpenAIDowngradeStateReprobe},
			{State: OpenAIDowngradeStateReprobe, ProbeMode: "sol_fallback"},
			{State: OpenAIDowngradeStatePendingReplace},
			{State: OpenAIDowngradeStateOnDuty, ProbeMode: "sol_fallback"},
		} {
			t.Run(status+"/"+initial.State+"/"+initial.ProbeMode, func(t *testing.T) {
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: status}
				repo := &downgradeProbeAccountRepoStub{account: account}
				store := &downgradeProbeStoreStub{}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					t.Fatal("disabled account must not be probed")
					return OpenAIDowngradeProbeResult{}
				}
				initial.AccountID = 1
				// 2026-09-17 起不可调度 status 不再静默跳过：不探（同旧），
				// 但排期后移让出队首，否则同桶活号被钉死在 rank-2。
				require.NoError(t, runner.processState(context.Background(), &initial, now))
				require.Zero(t, store.probeCalls)
				require.Equal(t, 1, store.saveCalls, "must reschedule to yield the queue head")
				require.Zero(t, store.eventCalls)
				require.Empty(t, store.proxyChanges)
				require.Empty(t, repo.schedulableCalls)
				minNext := now.Add(openAIDowngradeHalfOpenInterval)
				maxNext := now.Add(time.Duration(float64(openAIDowngradeHalfOpenInterval) * 1.25))
				require.True(t, !initial.NextProbeAt.Before(minNext) && !initial.NextProbeAt.After(maxNext),
					"disabled-account revisit %v outside spread window [%v, %v]",
					initial.NextProbeAt, minNext, maxNext)
			})
		}
	}
}

func TestProbeAccountLifecycleEligibility(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	proxyID, parentID := int64(3), int64(10)
	for _, tc := range []struct {
		name    string
		mutate  func(*Account)
		allowed bool
	}{
		{"active", func(*Account) {}, true},
		{"expired", func(a *Account) { a.ExpiresAt = timePtr(now.Add(-time.Second)); a.AutoPauseOnExpired = true }, false},
		{"expiry_boundary", func(a *Account) { a.ExpiresAt = &now; a.AutoPauseOnExpired = true }, false},
		{"future_expiry", func(a *Account) { a.ExpiresAt = timePtr(now.Add(time.Second)); a.AutoPauseOnExpired = true }, true},
		{"expiry_opt_out", func(a *Account) { a.ExpiresAt = &now }, true},
		{"shadow", func(a *Account) { a.ParentAccountID = &parentID }, false},
		{"other_platform", func(a *Account) { a.Platform = "other" }, false},
		{"non_oauth", func(a *Account) { a.Type = "apikey" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
				Status: StatusActive, Schedulable: true, ProxyID: &proxyID}
			tc.mutate(account)
			require.Equal(t, tc.allowed, isOpenAIDowngradeProbeAccountEligible(account, now))
			if tc.allowed {
				return
			}
			for _, mode := range []string{"normal", "qualification", "sol_fallback"} {
				store := &downgradeProbeStoreStub{}
				repo := &downgradeProbeAccountRepoStub{account: account}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				runner.now = func() time.Time { return now }
				runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
					t.Fatal("ineligible account must not reach transport")
					return OpenAIDowngradeProbeResult{}
				}
				state := OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStateReprobe,
					ProbeMode: mode, CurrentProxyID: &proxyID}
				require.NoError(t, runner.RunOnce(context.Background()))
				// 2026-09-17 起资格不符的活账号不再静默跳过：不探（同旧），
				// 但排期后移让出队首。
				require.NoError(t, runner.processState(context.Background(), &state, now))
				// 让位重排必经 Save，桩会把 state 存回 store.state；断言存回的
				// 是让位排期而非探针路径产物。
				require.NotNil(t, store.state)
				require.Equal(t, state.AccountID, store.state.AccountID)
				require.Zero(t, store.probeCalls)
				require.Equal(t, 1, store.saveCalls, "must reschedule to yield the queue head")
				require.Zero(t, store.eventCalls)
				require.Empty(t, store.proxyChanges)
				require.Empty(t, repo.schedulableCalls)
				minNext := now.Add(openAIDowngradeHalfOpenInterval)
				maxNext := now.Add(time.Duration(float64(openAIDowngradeHalfOpenInterval) * 1.25))
				require.True(t, !state.NextProbeAt.Before(minNext) && !state.NextProbeAt.After(maxNext),
					"ineligible-account revisit %v outside spread window [%v, %v]",
					state.NextProbeAt, minNext, maxNext)
			}
		})
	}
	require.False(t, isOpenAIDowngradeProbeAccountEligible(nil, now))
}

func TestReplacementRetryNeverChangesRouteAtSwapBudgetBoundary(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	original, escape := int64(3), int64(5)
	for _, tc := range []struct {
		name string
		age  time.Duration
	}{
		{"recent", time.Hour},
		{"before_boundary", openAIDowngradeSwapWindow - time.Nanosecond},
		{"at_boundary", openAIDowngradeSwapWindow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lastSwap := now.Add(-tc.age)
			account := &Account{ID: 1, ProxyID: &original}
			store := &downgradeProbeStoreStub{escapeProxyID: &escape}
			repo := &downgradeProbeAccountRepoStub{account: account}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return OpenAIDowngradeProbeResult{HTTPStatus: http.StatusServiceUnavailable}
			}
			state := &OpenAIDowngradeProbeState{AccountID: 1, State: OpenAIDowngradeStatePendingReplace,
				SwapCount7d: 2, LastSwapAt: &lastSwap, CurrentProxyID: &original, OriginalProxyID: &original}
			require.NoError(t, runner.retryReplacement(context.Background(), account, state, now))
			require.Empty(t, store.proxyChanges)
			require.Equal(t, 1, store.probeCalls)
			require.Equal(t, 2, state.SwapCount7d)
			require.Equal(t, lastSwap, *state.LastSwapAt)
			require.Equal(t, &original, state.CurrentProxyID)
			require.Equal(t, OpenAIDowngradeStateReprobe, state.State)
		})
	}
}

func TestOpenAIDowngradeProbeErrorStatusCircuitOpenStillRescued(t *testing.T) {
	openedAt := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	now := openedAt.Add(openAIDowngradeRecoveryWindow)
	originalProxyID := int64(3)
	escapeProxyID := int64(5)
	account := &Account{
		ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusError, Schedulable: false, ProxyID: &originalProxyID,
	}
	store := &downgradeProbeStoreStub{escapeProxyID: &escapeProxyID}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	runner.now = func() time.Time { return now }
	runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
		return OpenAIDowngradeProbeResult{
			TransportOK: true, AnswerCorrect: true,
			ReasoningTokens: downgradeProbeIntPtr(OpenAIDowngradeRecoveryReasoningMinimum),
		}
	}
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, State: OpenAIDowngradeStateCircuitOpen,
		ProbeMode: "normal", OriginalProxyID: &originalProxyID,
		CurrentProxyID: &originalProxyID, CircuitOpenedAt: &openedAt,
		NextProbeAt: now,
	}

	require.NoError(t, runner.processState(context.Background(), state, now))
	require.Equal(t, 1, store.probeCalls)
	require.Empty(t, store.proxyChanges)
	require.Equal(t, &originalProxyID, state.CurrentProxyID)
	require.Equal(t, OpenAIDowngradeStateReprobe, state.State)
}

func TestOpenAIDowngradeReplacementRetainsCurrentRoute(t *testing.T) {
	now := time.Now()
	original, current := int64(3), int64(5)
	account := &Account{ID: 1, ProxyID: &current}
	store := &downgradeProbeStoreStub{}
	repo := &downgradeProbeAccountRepoStub{account: account}
	runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{
		AccountID: 1, OriginalProxyID: &original, CurrentProxyID: &current,
	}
	require.NoError(t, runner.finishReplacement(context.Background(), state, now))
	require.Empty(t, store.proxyChanges)
	require.Equal(t, &current, state.CurrentProxyID)
	require.Equal(t, &current, account.ProxyID)
	require.Equal(t, OpenAIDowngradeStatePendingReplace, state.State)
}

func TestOpenAIDowngradeReprobeRejectsMissingOrStaleRoute(t *testing.T) {
	current, other := int64(3), int64(5)
	for _, proxyID := range []*int64{nil, &other} {
		store := &downgradeProbeStoreStub{escapeProxyID: &other}
		account := &Account{ID: 1, ProxyID: proxyID}
		repo := &downgradeProbeAccountRepoStub{account: account}
		runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
		state := &OpenAIDowngradeProbeState{AccountID: 1, CurrentProxyID: &current}
		before := *state
		require.ErrorIs(t, runner.beginReprobe(context.Background(), account, state, time.Now()), errOpenAIOAuthProxyUnavailable)
		require.Equal(t, before, *state)
		require.Empty(t, store.proxyChanges)
		require.Zero(t, store.probeCalls)
	}
}
