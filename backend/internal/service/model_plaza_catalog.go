package service

import "strings"

// Only saved, effective model names establish supply; GetModelMapping also
// injects built-in aliases that the operator may never have configured.
func plazaAccountModelNames(a *Account) []string {
	if a.IsOpenAIPassthroughEnabled() {
		return nil
	}
	var models []string
	mapping, _ := a.Credentials["model_mapping"].(map[string]any)
	for model, rawTarget := range mapping {
		target, ok := rawTarget.(string)
		if ok && strings.TrimSpace(target) != "" && !strings.Contains(model, "*") {
			models = append(models, model)
		}
	}
	return dedupeAndSortModelIDs(models)
}
