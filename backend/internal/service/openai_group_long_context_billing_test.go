//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIRecordUsageLongContextUsesOfficialTier(t *testing.T) {
	for _, resolverPresent := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, legacy := range []any{nil, false, true} {
				for _, config := range []GroupLongContextPricing{
					{}, {ThresholdTokens: 128000},
					{ThresholdTokens: 128000, ModelThresholds: map[string]int{"gpt-6.1-sol": 272000}},
				} {
					for _, totalInput := range []int{128001, 272000, 272001, 388359} {
						name := fmt.Sprintf("resolver=%t/group=%t/legacy=%v/config=%v/input=%d", resolverPresent, enabled, legacy, config, totalInput)
						t.Run(name, func(t *testing.T) {
							usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
							userRepo := &openAIRecordUsageUserRepoStub{}
							svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
							key := openAIRecordUsageAPIKeyWithGroup(svc, 1, enabled)
							key.Group.LongContextPricing = config
							key.Group.RateMultiplier = 0.6
							key.GroupID = i64p(key.Group.ID)
							if !resolverPresent {
								svc.resolver = nil
							}
							svc.channelService = newTestChannelServiceForStats(t, &Channel{ID: 1, Status: StatusActive}, key.Group.ID, PlatformOpenAI)
							extra := map[string]any{}
							if legacy != nil {
								extra[openAILongContextBillingEnabledKey] = legacy
							}
							account := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: extra}
							cached := totalInput - 119379
							err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
								Result: &OpenAIForwardResult{RequestID: name, Model: "gpt-6.1-sol", Duration: time.Second,
									Usage: OpenAIUsage{InputTokens: totalInput, CacheReadInputTokens: cached, OutputTokens: 930}},
								APIKey: key, User: &User{ID: 2}, Account: account,
							})
							require.NoError(t, err)
							log := usageRepo.lastLog
							require.NotNil(t, log)
							wantLong := (resolverPresent && enabled || legacy == true) && totalInput > 272000
							inFactor, outFactor := 1.0, 1.0
							if wantLong {
								inFactor, outFactor = 2, 1.5
							}
							total := (119379*2e-6+float64(cached)*0.1e-6)*inFactor + 930*10e-6*outFactor
							require.Equal(t, wantLong, log.LongContextBillingApplied)
							require.InDelta(t, total, log.TotalCost, 1e-10)
							require.InDelta(t, total*0.6, log.ActualCost, 1e-10)
							require.Equal(t, 1, userRepo.deductCalls)
							require.InDelta(t, log.ActualCost, userRepo.lastAmount, 1e-10)
							require.Equal(t, key.GroupID, log.GroupID)
							// Upstream accounting retains its independent account policy and catalog threshold.
							statsLong := legacy == true && totalInput > 272000
							statsIn, statsOut := 1.0, 1.0
							if statsLong {
								statsIn, statsOut = 2, 1.5
							}
							require.NotNil(t, log.AccountStatsCost)
							require.InDelta(t, (119379*2e-6+float64(cached)*0.1e-6)*statsIn+930*10e-6*statsOut, *log.AccountStatsCost, 1e-10)
						})
					}
				}
			}
		}
	}
}
