package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type quotaCutUsageRepoStub struct {
	UsageLogRepository
	stats *usagestats.AccountStats
	err   error
	calls int
}

func (s *quotaCutUsageRepoStub) GetAccountWindowStats(
	_context context.Context, _ int64, _ time.Time,
) (*usagestats.AccountStats, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.stats, nil
}

type quotaCutAccountRepoStub struct {
	AccountRepository
	account     *Account
	extraWrites []map[string]any
}

func (s *quotaCutAccountRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return s.account, nil
}

func (s *quotaCutAccountRepoStub) UpdateExtra(
	_ context.Context, _ int64, updates map[string]any,
) error {
	// 模拟生产 JSONB 语义：UpdateExtra 落库前会 json.Marshal，读回是
	// map[string]any/[]any/float64 形态，桩必须同样往返一次。
	payload, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	var roundTripped map[string]any
	if err := json.Unmarshal(payload, &roundTripped); err != nil {
		return err
	}
	s.extraWrites = append(s.extraWrites, roundTripped)
	if s.account != nil {
		if s.account.Extra == nil {
			s.account.Extra = make(map[string]any)
		}
		for key, value := range roundTripped {
			s.account.Extra[key] = value
		}
	}
	return nil
}

type quotaCutEventWriterStub struct {
	events []string
}

func (s *quotaCutEventWriterStub) AppendOpenAIDowngradeEvent(
	_ context.Context, _ int64, _ *int64, eventType string, _ map[string]any,
) error {
	s.events = append(s.events, eventType)
	return nil
}

// quotaCutExhaustedHeaders 构造 7d 打满的 x-codex-* 头（primary=周窗 100%）。
func quotaCutExhaustedHeaders() http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-reset-after-seconds", "3600")
	h.Set("x-codex-primary-window-minutes", "10080")
	h.Set("x-codex-secondary-used-percent", "40")
	h.Set("x-codex-secondary-reset-after-seconds", "1200")
	h.Set("x-codex-secondary-window-minutes", "300")
	return h
}

// quotaCutHistoryExtra 把记录做一次 JSON 往返，得到 accounts.extra 里
// JSONB 读回的真实形态（[]any/map[string]any/float64/RFC3339 字符串）。
func quotaCutHistoryExtra(t *testing.T, records ...openAI7dExhaustionRecord) []any {
	t.Helper()
	var out []any
	require.NoError(t, json.Unmarshal([]byte(marshalOpenAI7dExhaustionHistory(records)), &out))
	return out
}

func TestNormalizeOpenAI7dExhaustionHistoryRoundTrip(t *testing.T) {
	base := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	records := []openAI7dExhaustionRecord{
		{ExhaustedAt: base, WindowStart: base.Add(-7 * 24 * time.Hour),
			WindowResetAt: base.Add(time.Hour), Cost: 12.5, Tokens: 900000, Requests: 77},
		{ExhaustedAt: base.Add(-72 * time.Hour), WindowStart: base.Add(-10 * 24 * time.Hour),
			WindowResetAt: base.Add(-48 * time.Hour), Cost: 11.0, Tokens: 800000, Requests: 70},
	}
	parsed := normalizeOpenAI7dExhaustionHistory(quotaCutHistoryExtra(t, records...))
	require.Len(t, parsed, 2)
	require.Equal(t, records[0].ExhaustedAt.Unix(), parsed[0].ExhaustedAt.Unix())
	require.InDelta(t, records[0].Cost, parsed[0].Cost, 1e-9)
	require.Equal(t, records[0].Tokens, parsed[0].Tokens)
	require.Equal(t, records[0].Requests, parsed[0].Requests)
	require.Equal(t, records[1].WindowResetAt.Unix(), parsed[1].WindowResetAt.Unix())

	require.Nil(t, normalizeOpenAI7dExhaustionHistory(nil))
	require.Nil(t, normalizeOpenAI7dExhaustionHistory("not-a-slice"))
	// 缺 exhausted_at 的损坏条目只跳过自身。
	require.Empty(t, normalizeOpenAI7dExhaustionHistory([]any{
		map[string]any{"cost": 1.0},
	}))
}

func TestAppendOpenAI7dExhaustionRecordDedupAndCap(t *testing.T) {
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	record := openAI7dExhaustionRecord{
		ExhaustedAt: now, WindowStart: now.Add(-7 * 24 * time.Hour),
		WindowResetAt: now.Add(time.Hour), Cost: 10,
	}

	// 同窗持续 429：打满时刻仍在上条记录窗口内 → 不重复记录。
	sameWindow := record
	sameWindow.ExhaustedAt = now.Add(10 * time.Minute)
	_, appended := appendOpenAI7dExhaustionRecord([]openAI7dExhaustionRecord{record}, sameWindow)
	require.False(t, appended)

	// 429 风暴护栏：窗口虽已过 reset，但距上条记录不足 1h → 不记录。
	burst := record
	burst.ExhaustedAt = now.Add(30 * time.Minute)
	burst.WindowResetAt = now.Add(10 * time.Minute)
	_, appended = appendOpenAI7dExhaustionRecord([]openAI7dExhaustionRecord{burst}, sameWindow)
	require.False(t, appended)

	// 新窗口打满（上条已过 reset 且超 1h）→ 记录，最新在前。
	nextWindow := record
	nextWindow.ExhaustedAt = now.Add(48 * time.Hour)
	nextWindow.WindowStart = now.Add(41 * 24 * time.Hour)
	nextWindow.WindowResetAt = now.Add(49 * time.Hour)
	nextWindow.Cost = 7
	updated, appended := appendOpenAI7dExhaustionRecord([]openAI7dExhaustionRecord{record}, nextWindow)
	require.True(t, appended)
	require.Len(t, updated, 2)
	require.Equal(t, 7.0, updated[0].Cost)

	// 上限 4 条：新记录前插，最旧被裁掉。
	full := make([]openAI7dExhaustionRecord, 0, openAIQuotaCutHistoryMax)
	for i := 0; i < openAIQuotaCutHistoryMax; i++ {
		item := record
		item.ExhaustedAt = now.Add(-time.Duration(i+1) * 48 * time.Hour)
		item.WindowResetAt = item.ExhaustedAt.Add(time.Hour)
		full = append(full, item)
	}
	updated, appended = appendOpenAI7dExhaustionRecord(full, record)
	require.True(t, appended)
	require.Len(t, updated, openAIQuotaCutHistoryMax)
	require.Equal(t, record.ExhaustedAt.Unix(), updated[0].ExhaustedAt.Unix())
}

func TestDetectOpenAIQuotaCutBaselines(t *testing.T) {
	now := time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)
	newRecord := func(cost float64, offset time.Duration) openAI7dExhaustionRecord {
		at := now.Add(-offset)
		return openAI7dExhaustionRecord{
			ExhaustedAt: at, WindowResetAt: at.Add(time.Hour), Cost: cost,
		}
	}

	// 基线不足 2 窗：不判。
	require.Nil(t, detectOpenAIQuotaCut([]openAI7dExhaustionRecord{
		newRecord(7, 0), newRecord(10, 48*time.Hour),
	}))

	// 中位 10：现窗 7.0（ratio 0.7 < 0.75）→ 告警；8.0（0.8）→ 不告警。
	alerting := []openAI7dExhaustionRecord{
		newRecord(7.0, 0), newRecord(10, 48*time.Hour), newRecord(10, 96*time.Hour),
	}
	verdict := detectOpenAIQuotaCut(alerting)
	require.NotNil(t, verdict)
	require.InDelta(t, 10.0, verdict.MedianPrior, 1e-9)
	require.InDelta(t, 0.7, verdict.Ratio, 1e-9)

	calm := []openAI7dExhaustionRecord{
		newRecord(8.0, 0), newRecord(10, 48*time.Hour), newRecord(10, 96*time.Hour),
	}
	require.Nil(t, detectOpenAIQuotaCut(calm))

	// 偶数基线取两中位均值：{8,12} 中位 10，现窗 7.0 → 告警。
	even := []openAI7dExhaustionRecord{
		newRecord(7.0, 0), newRecord(8, 48*time.Hour), newRecord(12, 96*time.Hour),
	}
	verdict = detectOpenAIQuotaCut(even)
	require.NotNil(t, verdict)
	require.InDelta(t, 10.0, verdict.MedianPrior, 1e-9)

	// 基线全 0（无信号）→ 不判。
	require.Nil(t, detectOpenAIQuotaCut([]openAI7dExhaustionRecord{
		newRecord(0, 0), newRecord(0, 48*time.Hour), newRecord(0, 96*time.Hour),
	}))
}

func TestNoteOpenAI7dExhaustionRecordsAndAlerts(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 1, Platform: PlatformOpenAI, Extra: map[string]any{}}
	prior := func(offset time.Duration, cost float64) openAI7dExhaustionRecord {
		at := now.Add(-offset)
		return openAI7dExhaustionRecord{
			ExhaustedAt: at, WindowStart: at.Add(-7 * 24 * time.Hour),
			WindowResetAt: at.Add(time.Hour), Cost: cost, Tokens: 900000, Requests: 70,
		}
	}
	account.Extra[openAI7dExhaustionHistoryExtraKey] = quotaCutHistoryExtra(t,
		prior(72*time.Hour, 10.0), prior(7*24*time.Hour, 10.0))

	usageRepo := &quotaCutUsageRepoStub{stats: &usagestats.AccountStats{
		Cost: 7.0, Tokens: 640000, Requests: 42,
	}}
	accountRepo := &quotaCutAccountRepoStub{account: account}
	events := &quotaCutEventWriterStub{}
	svc := NewRateLimitService(accountRepo, usageRepo, nil, nil, nil)
	svc.SetOpenAIQuotaCutEventWriter(events)

	svc.noteOpenAI7dExhaustion(context.Background(), account, quotaCutExhaustedHeaders())

	require.Equal(t, 1, usageRepo.calls)
	require.Len(t, accountRepo.extraWrites, 1)
	updated := normalizeOpenAI7dExhaustionHistory(
		accountRepo.extraWrites[0][openAI7dExhaustionHistoryExtraKey])
	require.Len(t, updated, 3)
	require.InDelta(t, 7.0, updated[0].Cost, 1e-9)
	require.Equal(t, []string{OpenAIDowngradeEventQuotaCut}, events.events)

	// 同窗第二个 429：不重复记录、不重复告警。
	svc.noteOpenAI7dExhaustion(context.Background(), account, quotaCutExhaustedHeaders())
	require.Equal(t, 2, usageRepo.calls)
	require.Len(t, accountRepo.extraWrites, 1)
	require.Len(t, events.events, 1)
}

func TestNoteOpenAI7dExhaustionIgnores5hOnlyExhaustion(t *testing.T) {
	// primary 是 5h 窗（300 分钟）打满、7d(secondary) 只有 40% → 不记录。
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "100")
	h.Set("x-codex-primary-reset-after-seconds", "1200")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-secondary-used-percent", "40")
	h.Set("x-codex-secondary-reset-after-seconds", "3600")
	h.Set("x-codex-secondary-window-minutes", "10080")

	usageRepo := &quotaCutUsageRepoStub{stats: &usagestats.AccountStats{Cost: 7}}
	accountRepo := &quotaCutAccountRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI}}
	events := &quotaCutEventWriterStub{}
	svc := NewRateLimitService(accountRepo, usageRepo, nil, nil, nil)
	svc.SetOpenAIQuotaCutEventWriter(events)

	svc.noteOpenAI7dExhaustion(context.Background(), accountRepo.account, h)
	require.Zero(t, usageRepo.calls)
	require.Empty(t, accountRepo.extraWrites)
	require.Empty(t, events.events)
}

func TestNoteOpenAI7dExhaustionWithoutEventWriterStillRecords(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Extra: map[string]any{}}
	usageRepo := &quotaCutUsageRepoStub{stats: &usagestats.AccountStats{
		Cost: 10.0, Tokens: 900000, Requests: 70,
	}}
	accountRepo := &quotaCutAccountRepoStub{account: account}
	svc := NewRateLimitService(accountRepo, usageRepo, nil, nil, nil)

	// 无事件写出器（nil）时不得 panic，历史照常落库。
	svc.noteOpenAI7dExhaustion(context.Background(), account, quotaCutExhaustedHeaders())
	require.Len(t, accountRepo.extraWrites, 1)
}
