package service

import "strings"

// Only saved, effective model names establish supply; GetModelMapping also
// injects built-in aliases that the operator may never have configured.
func plazaAccountModelNames(a *Account) []string {
	if a.IsOpenAIPassthroughEnabled() {
		return nil
	}
	mapping, _ := a.Credentials["model_mapping"].(map[string]any)
	models := make([]string, 0, len(mapping))
	for model, rawTarget := range mapping {
		target, ok := rawTarget.(string)
		if ok && strings.TrimSpace(target) != "" && !strings.Contains(model, "*") {
			models = append(models, model)
		}
	}
	return models
}
