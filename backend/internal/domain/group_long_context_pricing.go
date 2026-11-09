package domain

// GroupLongContextPricing changes thresholds, never model rates or exemptions.
// A zero default inherits the model threshold; model keys are exact billing names.
type GroupLongContextPricing struct {
	ThresholdTokens int            `json:"threshold_tokens,omitempty"`
	ModelThresholds map[string]int `json:"model_thresholds,omitempty"`
}
