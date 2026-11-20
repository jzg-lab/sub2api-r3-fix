package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGeminiFallbackRatesFollowOfficialCatalog(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	for _, tc := range []struct {
		model                string
		input, output, cache float64
	}{
		{"gemini-3.6-flash", .75e-6, 3.75e-6, .075e-6},
		{"gemini-4-argon", 2e-6, 10e-6, .1e-6},
	} {
		p, err := svc.getModelPricingAt(tc.model, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.InDelta(t, tc.input, p.InputPricePerToken, 1e-15)
		require.InDelta(t, tc.output, p.OutputPricePerToken, 1e-15)
		require.InDelta(t, tc.cache, p.CacheReadPricePerToken, 1e-15)
	}
}
