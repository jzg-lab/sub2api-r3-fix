package service

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
)

// AccountRecentStats counts real text forwarding attempts, independently of billing.
type AccountRecentStats struct {
	Successes           int64 `json:"successes"`
	Failures            int64 `json:"failures"`
	CacheReadTokens     int64 `json:"-"`
	InputTokens         int64 `json:"-"`
	TTFTSumMs           int64 `json:"-"`
	TTFTCount           int64 `json:"latency_samples"`
	LastObservedAt      int64 `json:"last_observed_at"`
	ConsecutiveFailures int64 `json:"consecutive_failures"`
}

type AccountAttemptObservation struct {
	Success         bool
	CacheReadTokens int64
	InputTokens     int64
	FirstTokenMs    *int
}

type AccountRecentStatsCache interface {
	RecordAccountAttempt(context.Context, int64, AccountAttemptObservation, time.Time) error
	GetAccountRecentStats(context.Context, []int64, time.Time) (map[int64]AccountRecentStats, error)
}

// Reliability shrinks sparse samples towards a neutral prior; recent consecutive
// failures outweigh old successes, but never permanently exclude an idle account.
func (s AccountRecentStats) Reliability(now time.Time) float64 {
	score := float64(s.Successes+9) / float64(s.Successes+s.Failures+10)
	if s.LastObservedAt > 0 && now.Unix()-s.LastObservedAt >= 0 && now.Unix()-s.LastObservedAt < 60 && s.ConsecutiveFailures > 0 {
		score = math.Min(score, 1-0.15*float64(min(s.ConsecutiveFailures, 5)))
	}
	return score
}

type AccountRecentStatsView struct {
	AccountRecentStats
	Attempts     int64    `json:"attempts"`
	SuccessRate  *float64 `json:"success_rate"`
	CacheHitRate *float64 `json:"cache_hit_rate"`
	TTFTAvgMs    *float64 `json:"ttft_avg_ms"`
}

func (s AccountRecentStats) View() AccountRecentStatsView {
	v := AccountRecentStatsView{AccountRecentStats: s, Attempts: s.Successes + s.Failures}
	if v.Attempts > 0 {
		rate := float64(s.Successes) / float64(v.Attempts)
		v.SuccessRate = &rate
	}
	if s.InputTokens > 0 {
		rate := math.Min(1, float64(s.CacheReadTokens)/float64(s.InputTokens))
		v.CacheHitRate = &rate
	}
	if s.TTFTCount > 0 {
		ms := float64(s.TTFTSumMs) / float64(s.TTFTCount)
		v.TTFTAvgMs = &ms
	}
	return v
}

func LoadAccountRecentStats(ctx context.Context, cache GatewayCache, ids []int64) (map[int64]AccountRecentStats, error) {
	store, ok := cache.(AccountRecentStatsCache)
	if !ok || len(ids) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	return store.GetAccountRecentStats(ctx, ids, time.Now())
}

type recentAccountStatsKey struct{}

func withRecentAccountStats(ctx context.Context, cache GatewayCache, accounts []Account) context.Context {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	stats, _ := LoadAccountRecentStats(ctx, cache, ids)
	return context.WithValue(ctx, recentAccountStatsKey{}, stats)
}

func recentAccountReliability(ctx context.Context, id int64) float64 {
	stats, _ := ctx.Value(recentAccountStatsKey{}).(map[int64]AccountRecentStats)
	return stats[id].Reliability(time.Now())
}

func sortByRecentReliability(ctx context.Context, accounts []*Account) {
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority != accounts[j].Priority {
			return accounts[i].Priority < accounts[j].Priority
		}
		return recentAccountReliability(ctx, accounts[i].ID) > recentAccountReliability(ctx, accounts[j].ID)
	})
}

func filterByRecentReliability(ctx context.Context, accounts []accountWithLoad) []accountWithLoad {
	best := -1.0
	var selected []accountWithLoad
	for _, account := range accounts {
		score := recentAccountReliability(ctx, account.account.ID)
		if score > best {
			best = score
			selected = selected[:0]
		}
		if score == best {
			selected = append(selected, account)
		}
	}
	return selected
}

func accountAttemptFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var failure *UpstreamFailoverError
	if errors.As(err, &failure) {
		return !failure.RequestScopedTransient && failure.Scope != GatewayFailureScopeRequest &&
			(failure.StatusCode >= 500 || failure.StatusCode == 429 || failure.StatusCode == 401 || failure.StatusCode == 403 || failure.IsCredentialFailure() || failure.IsProxyChainFailure())
	}
	var network net.Error
	var stream *openAIUpstreamStreamReadError
	var sse *sseStreamErrorEventError
	return errors.As(err, &network) || errors.As(err, &stream) || errors.As(err, &sse) || errors.Is(err, io.ErrUnexpectedEOF)
}

func recordAccountAttempt(c *gin.Context, cache GatewayCache, account *Account, observation AccountAttemptObservation, err error) {
	store, ok := cache.(AccountRecentStatsCache)
	if !ok || account == nil || c == nil || c.Request.Context().Err() != nil || GetOpsCyberPolicy(c) != nil {
		return
	}
	streamErr, exists := GetOpsStreamError(c)
	// Ops keeps historical WS failures; only the current turn describes this attempt.
	if GetOpenAIClientTransport(c) == OpenAIClientTransportWS && streamErr.Turn != c.GetInt(OpsStreamTurnKey) {
		exists = false
	}
	if exists && streamErr.IntendedStatus >= 400 {
		if streamErr.IntendedStatus < 500 && streamErr.IntendedStatus != 429 {
			return
		}
		observation.Success = false
	} else if err != nil && !accountAttemptFailure(err) {
		return
	}
	// Billing substitutions are not evidence of upstream cache hits.
	if IsForceCacheBilling(c.Request.Context()) {
		observation.CacheReadTokens, observation.InputTokens = 0, 0
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 50*time.Millisecond)
	defer cancel()
	_ = store.RecordAccountAttempt(ctx, account.ID, observation, time.Now())
}

func (s *GatewayService) ObserveAccountAttempt(c *gin.Context, account *Account, result *ForwardResult, err error, forceCacheBilling bool) {
	if s == nil || (result == nil && err == nil) || (result != nil && (result.ClientDisconnect || result.ImageCount > 0 || result.AudioUsage != nil)) {
		return
	}
	o := AccountAttemptObservation{Success: err == nil}
	if result != nil && o.Success {
		o.CacheReadTokens = int64(max(0, result.Usage.CacheReadInputTokens))
		o.InputTokens = int64(max(0, result.Usage.InputTokens)+max(0, result.Usage.CacheCreationInputTokens)) + o.CacheReadTokens
		o.FirstTokenMs = result.FirstTokenMs
	}
	if forceCacheBilling {
		o.CacheReadTokens, o.InputTokens = 0, 0
	}
	recordAccountAttempt(c, s.cache, account, o, err)
}

func (s *OpenAIGatewayService) ObserveAccountAttempt(c *gin.Context, account *Account, result *OpenAIForwardResult, err error) {
	if s == nil || (result == nil && err == nil) || (result != nil && (result.ClientDisconnect || result.ImageCount > 0 || result.VideoCount > 0 || result.AudioUsage != nil)) {
		return
	}
	o := AccountAttemptObservation{Success: err == nil && result.SucceededForScheduling()}
	if result != nil && o.Success {
		o.CacheReadTokens = int64(max(0, result.Usage.CacheReadInputTokens))
		// OpenAI input_tokens includes cached tokens; usage persistence uses this same normalization.
		o.InputTokens = int64(max(max(0, result.Usage.InputTokens), max(0, result.Usage.CacheCreationInputTokens)+max(0, result.Usage.CacheReadInputTokens)))
		o.FirstTokenMs = result.FirstTokenMs
	}
	recordAccountAttempt(c, s.cache, account, o, err)
}
