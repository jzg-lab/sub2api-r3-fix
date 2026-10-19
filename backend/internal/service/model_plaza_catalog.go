package service

import "strings"

// plazaAccountModelNames uses the same built-in catalogs and account mappings
// as model discovery. Pricing catalogs only expand wildcards or providers with
// no built-in list; a price entry alone is not evidence of account support.
func (s *ModelPlazaService) plazaAccountModelNames(a *Account, catalogs map[string][]string) []string {
	var models []string
	switch a.Platform {
	case PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity,
		PlatformGrok, PlatformOpenCodeGo, PlatformTypeSafe:
		models = defaultModelsListCandidateIDs(a.Platform)
	}
	expandCatalog := len(models) == 0
	if !a.IsOpenAIPassthroughEnabled() {
		for model := range a.GetModelMapping() {
			if strings.Contains(model, "*") {
				expandCatalog = true
			} else {
				models = append(models, model)
			}
		}
	}
	if snapshot := a.GetUpstreamModelMetadataSnapshot(); snapshot != nil {
		for model := range snapshot.Models {
			models = append(models, model)
		}
	}
	if expandCatalog && s.pricingService != nil {
		catalog, ok := catalogs[a.Platform]
		if !ok {
			catalog = s.pricingService.ListModelNamesByProvider(a.Platform)
			catalogs[a.Platform] = catalog
		}
		models = append(models, catalog...)
	}
	return dedupeAndSortModelIDs(models)
}
