package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupLongContextPricingRequest(t *testing.T) {
	var create CreateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{"long_context_pricing":{"threshold_tokens":128000,"model_thresholds":{"model-a":256000}}}`), &create))
	require.Equal(t, 128000, create.LongContextPricing.ThresholdTokens)
	require.Equal(t, 256000, create.LongContextPricing.ModelThresholds["model-a"])
	var update UpdateGroupRequest
	require.NoError(t, json.Unmarshal([]byte(`{}`), &update))
	require.Nil(t, update.LongContextPricing)
	require.NoError(t, json.Unmarshal([]byte(`{"long_context_pricing":{}}`), &update))
	require.NotNil(t, update.LongContextPricing)
	require.Zero(t, update.LongContextPricing.ThresholdTokens)
	require.Error(t, json.Unmarshal([]byte(`{"long_context_pricing":{"threshold_tokens":128.5}}`), &update))
}
