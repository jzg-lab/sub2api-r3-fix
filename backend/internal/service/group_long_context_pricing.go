package service

import (
	"math"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type GroupLongContextPricing = domain.GroupLongContextPricing

func normalizeGroupLongContextPricing(config GroupLongContextPricing) (GroupLongContextPricing, error) {
	invalid := func() (GroupLongContextPricing, error) {
		return GroupLongContextPricing{}, infraerrors.BadRequest("INVALID_GROUP_LONG_CONTEXT_PRICING", "thresholds must be positive whole K tokens (1K = 1000 tokens), within int32; model names must be unique exact billing names")
	}
	valid := func(tokens int) bool { return tokens > 0 && tokens <= math.MaxInt32 && tokens%1000 == 0 }
	if config.ThresholdTokens != 0 && !valid(config.ThresholdTokens) {
		return invalid()
	}
	out := GroupLongContextPricing{ThresholdTokens: config.ThresholdTokens}
	if len(config.ModelThresholds) > 0 {
		out.ModelThresholds = make(map[string]int, len(config.ModelThresholds))
	}
	for model, threshold := range config.ModelThresholds {
		model = normalizeChannelPricingModelName(model)
		if model == "" || len(model) > 256 || strings.ContainsAny(model, "*?\t\r\n ") || !valid(threshold) {
			return invalid()
		}
		if _, duplicate := out.ModelThresholds[model]; duplicate {
			return invalid()
		}
		out.ModelThresholds[model] = threshold
	}
	return out, nil
}
