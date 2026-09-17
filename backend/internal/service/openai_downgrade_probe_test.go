package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type downgradeProbeAccountRepoStub struct {
	AccountRepository
	account           *Account
	getByIDErr        error
	schedulableErr    error
	schedulableCalls  []bool
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

func (s *downgradeProbeAccountRepoStub) ListByPlatform(context.Context, string) ([]Account, error) {
	if s.account == nil {
		return nil, nil
	}
	return []Account{*s.account}, nil
}

func (s *downgradeProbeAccountRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
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
	state          *OpenAIDowngradeProbeState
	ensureNextAt   time.Time
	due            []OpenAIDowngradeProbeState
	saveCalls      int
	probeCalls     int
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
	_ context.Context, accountID int64, proxyID *int64, nextAt time.Time,
) (*OpenAIDowngradeProbeState, error) {
	s.ensureNextAt = nextAt
	if s.state == nil {
		s.state = &OpenAIDowngradeProbeState{
			AccountID: accountID, State: OpenAIDowngradeStateOnDuty,
			CurrentProxyID: proxyID, OriginalProxyID: proxyID, NextProbeAt: nextAt,
		}
	}
	return s.state, nil
}

func (s *downgradeProbeStoreStub) ListDueOpenAIDowngradeStates(context.Context, time.Time, int) ([]OpenAIDowngradeProbeState, error) {
	return s.due, nil
}

func (s *downgradeProbeStoreStub) ReconcileOpenAIRateLimitProbeSchedules(context.Context, time.Time, time.Duration) (int64, error) {
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

func (s *downgradeProbeStoreStub) RecordOpenAIDowngradeProbe(context.Context, *OpenAIDowngradeProbeResult) error {
	s.probeCalls++
	return nil
}

// GetOpenAIDowngradeState 供代际失配重试(retryStaleCommit)读取新鲜状态行:
// 未预置 state 时返回 nil,重试按"状态行不可用"让位。
func (s *downgradeProbeStoreStub) GetOpenAIDowngradeState(context.Context, int64) (*OpenAIDowngradeProbeState, error) {
	return s.state, nil
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

func TestApplyOpenAIDowngradeProbeResultRequiresTwoFailures(t *testing.T) {
	state := OpenAIDowngradeProbeState{State: OpenAIDowngradeStateOnDuty}
	failed := OpenAIDowngradeProbeResult{
		TransportOK:     true,
		AnswerCorrect:   false,
		ReasoningTokens: downgradeProbeIntPtr(516),
	}

	first := ApplyOpenAIDowngradeProbeResult(state, failed, time.Now())
	require.False(t, first.Circuit)
	require.Equal(t, OpenAIDowngradeStateOnDuty, first.NextState)

	state.ConsecutiveFailures = 1
	second := ApplyOpenAIDowngradeProbeResult(state, failed, time.Now())
	require.True(t, second.Circuit)
	require.Equal(t, OpenAIDowngradeStateCircuitOpen, second.NextState)
	require.Equal(t, OpenAIDowngradeEventCircuitOpen, second.EventType)
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

	require.True(t, low.IsDegraded())
	require.False(t, boundary.IsDegraded())
	require.False(t, boundary.IsRecovered())
	require.True(t, recovered.IsRecovered())
}

func TestOpenAIDowngradeProbeTruncationFingerprintsSplitByAnswer(t *testing.T) {
	// 2026-09-15 用户裁定：指纹+答对=中性（预算截断未伤结论，1034/1035 实测
	// rt 恒落 1552 且答案正确）；指纹+答错=降智。深截断（<800，如 516）即使
	// 答对也仍判负——低到及格线下的推理量不可能支撑可信结论。
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
		if tokens < OpenAIDowngradeFailureReasoningThreshold {
			require.Truef(t, correct.IsDegraded(), "tokens=%d below failure threshold stays degraded even when correct", tokens)
		} else {
			require.Falsef(t, correct.IsDegraded(), "tokens=%d correct answer should be neutral", tokens)
		}
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
	}

	require.ErrorContains(t, runner.processState(context.Background(), state, time.Now()), "proxy")
	require.Zero(t, store.probeCalls)
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

func TestOpenAIDowngradeProbeQualificationArmPausesAccountBeforeBinding(t *testing.T) {
	now := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	// 新上传的号：schedulable=true、未绑桶。r15b 的调度闸只挡「未绑桶」，
	// 资格流程首轮就会绑桶——若不在 arm 时落下认证闸，绑上的瞬间号即进调度。
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
	require.Equal(t, []bool{false}, repo.schedulableCalls,
		"arming qualification must pause the account before first binding")
	require.False(t, account.Schedulable)
	require.NotNil(t, store.state)
	require.Equal(t, "qualification", store.state.ProbeMode)
	require.True(t, !store.state.NextProbeAt.After(now),
		"qualification must be probed immediately, got %v", store.state.NextProbeAt)
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
	require.Empty(t, repo.rateLimitedResets, "reset distance alone does not identify the window")
	require.Equal(t, "unknown_window", store.eventDetails[0]["class"])
}

func TestProbe429ResetTimeFloorsAndCaps(t *testing.T) {
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

	// 重置点离谱远（90 天）：cap 到 now+8d 防脏数据把账号钉死；r17 起排期
	// 取 min(cap+错峰, 每日稀疏复查)——复查侧（22-27.5h）恒早于 8d，账号
	// 每天仍有一针检测官方/手动提前重置，cap 只钳持有语义不再钉死探针节奏。
	runner, state, now = newFixture(nowAdd(time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC), 90*24*time.Hour))
	require.NoError(t, runner.processState(context.Background(), state, now))
	lo = now.Add(openAIDowngradeRateLimitRecheckInterval)
	hi = now.Add(time.Duration(float64(openAIDowngradeRateLimitRecheckInterval) * 1.25))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"capped backoff %v outside daily recheck window [%v, %v]", state.NextProbeAt, lo, hi)
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

// r17 稀疏复查五轨回归（2026-09-16 用户裁定「重置不是固定的，有时候可以
// 手动重置」）：长持有每天最多一针随机复查、5h 短窗行为不变、近重置点收敛、
// 持有中的无信息 429 不进风暴闸、复查 200 按 CAS 清除持有回岗。
func TestProbe429SparseRecheckLongHoldGetsDailyCadence(t *testing.T) {
	// 7d 周限、重置点 7 天后（cap 8d 内不截断）：min 规则取复查侧（22-27.5h），
	// 不再死等重置点；周限持有照常单调延长（SetRateLimitedIfLater）。
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
	recheckLo := now.Add(openAIDowngradeRateLimitRecheckInterval)
	recheckHi := now.Add(time.Duration(float64(openAIDowngradeRateLimitRecheckInterval) * 1.25))
	require.True(t, !state.NextProbeAt.Before(recheckLo) && !state.NextProbeAt.After(recheckHi),
		"long-hold recheck %v outside daily window [%v, %v]", state.NextProbeAt, recheckLo, recheckHi)
	require.Equal(t, true, store.eventDetails[0]["recheck"], "recheck-scheduled deferral must be labeled")
	require.Len(t, repo.rateLimitedResets, 1, "7d window must still extend the account hold")
}

func TestProbe429ShortWindowCadenceUnchangedByRecheck(t *testing.T) {
	// 重置点 1 小时后（忙时窗口语义）：复查侧（≥22h）恒晚于重置点+错峰，
	// min 规则取重置点一侧，r15g 行为不变——短窗本就该在重置后立刻回来。
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
	// 重置点落在复查窗内（now+23h）：min 两侧都在 [22h, 27.5h] 包络内——
	// 复查抽得早取复查、晚则取重置点+错峰，无空档也无双重等待。
	now := time.Date(2026, 9, 16, 22, 0, 0, 0, time.UTC)
	resetAt := now.Add(23 * time.Hour)
	runner := NewOpenAIDowngradeProbeRunner(&downgradeProbeStoreStub{}, &downgradeProbeAccountRepoStub{}, nil, nil, nil, nil)
	state := &OpenAIDowngradeProbeState{AccountID: 1, NextProbeAt: now}
	handled, err := runner.applyRateLimitDeferral(context.Background(), nil, state,
		OpenAIDowngradeProbeResult{HTTPStatus: http.StatusTooManyRequests,
			RateLimitResetAt: &resetAt, RateLimitWindow: "unknown_window"}, now)
	require.True(t, handled)
	require.NoError(t, err)
	lo := now.Add(openAIDowngradeRateLimitRecheckInterval)
	hi := now.Add(time.Duration(float64(openAIDowngradeRateLimitRecheckInterval) * 1.25))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"converged backoff %v outside envelope [%v, %v]", state.NextProbeAt, lo, hi)
}

func TestProbe429WhileHeldSkipsStormGate(t *testing.T) {
	// 账号已在限流持有中（reset 未到）再吃无时间信息 429：不进 1 小时风暴闸、
	// 不计数（限流非降智证据），锚定持有走稀疏复查。未持有账号保持原语义。
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
	lo := now.Add(openAIDowngradeRateLimitRecheckInterval)
	hi := now.Add(time.Duration(float64(openAIDowngradeRateLimitRecheckInterval) * 1.25))
	require.True(t, !state.NextProbeAt.Before(lo) && !state.NextProbeAt.After(hi),
		"held-account no-info 429 must use sparse recheck, got %v", state.NextProbeAt)
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

	body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":1790000000}}`)
	resetAt = probeOpenAI429ResetTime(http.Header{}, body)
	require.NotNil(t, resetAt)
	require.Equal(t, int64(1790000000), resetAt.Unix())

	require.Nil(t, probeOpenAI429ResetTime(http.Header{}, []byte(`{"error":{"type":"rate_limited"}}`)))
}

func TestProbe429WindowClassificationRequiresExplicitEvidence(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		window string
		delay  time.Duration
		weekly bool
	}{
		{"short", "5h_window", 2 * time.Hour, false},
		{"week", "7d_window", 72 * time.Hour, true},
		{"week_near_reset", "7d_window", time.Hour, true},
		{"unknown_near", "", time.Hour, false},
		{"unknown_far", "", 72 * time.Hour, false},
		{"invalid_label", "weekly", 72 * time.Hour, false},
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
			if tc.weekly {
				require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets)
			} else {
				require.Empty(t, repo.rateLimitedResets)
			}
			// r17 稀疏复查：远重置点（复查侧恒早于重置点+错峰）不死等重置点，
			// 取每日随机复查；近重置点仍取重置点一侧（After(resetAt)）。
			if tc.delay > 27*time.Hour {
				recheckLo := now.Add(openAIDowngradeRateLimitRecheckInterval)
				recheckHi := now.Add(time.Duration(float64(openAIDowngradeRateLimitRecheckInterval) * 1.25))
				require.True(t, !state.NextProbeAt.Before(recheckLo) && !state.NextProbeAt.After(recheckHi),
					"far reset must use sparse recheck, got %v", state.NextProbeAt)
			} else {
				require.True(t, state.NextProbeAt.After(resetAt))
			}
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
			repo := &downgradeProbeAccountRepoStub{}
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
			require.NoError(t, runner.startSolFallback(context.Background(), &Account{ID: 1}, state, now))
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
			repo := &downgradeProbeAccountRepoStub{}
			runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
			runner.probeFn = func(context.Context, *Account, string) OpenAIDowngradeProbeResult {
				return tc.result
			}
			state := &OpenAIDowngradeProbeState{
				AccountID: 1, State: OpenAIDowngradeStateReprobe,
				ConsecutiveFailures: 2, NextProbeAt: now,
			}
			require.NoError(t, runner.startSolFallback(context.Background(), &Account{ID: 1}, state, now))
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
		for _, window := range []string{"", "5h_window", "7d_window"} {
			t.Run(initial.State+"/"+initial.ProbeMode+"/"+window, func(t *testing.T) {
				proxyID := int64(3)
				account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Status: StatusActive, Schedulable: true, ProxyID: &proxyID}
				store := &downgradeProbeStoreStub{}
				repo := &downgradeProbeAccountRepoStub{account: account}
				runner := NewOpenAIDowngradeProbeRunner(store, repo, nil, nil, nil, nil)
				resetAt := now.Add(time.Hour)
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
				if window == "7d_window" {
					require.Equal(t, []time.Time{resetAt}, repo.rateLimitedResets)
				} else {
					require.Empty(t, repo.rateLimitedResets)
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
			runner := NewOpenAIDowngradeProbeRunner(store, nil, nil, nil, nil, nil)
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
