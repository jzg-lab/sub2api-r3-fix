package service

import (
	"maps"
	"strings"
)

// StripOpenAIRescueManagedExtra protects rescue state at admin write boundaries.
// Internal rescue writers use AccountRepository.UpdateExtra directly.
func StripOpenAIRescueManagedExtra(incoming map[string]any) map[string]any {
	extra := maps.Clone(incoming)
	for key := range extra {
		if strings.HasPrefix(key, "openai_rescue_") {
			delete(extra, key)
		}
	}
	return extra
}

func PreserveOpenAIRescueManagedExtra(current, incoming map[string]any) map[string]any {
	extra := StripOpenAIRescueManagedExtra(incoming)
	if extra == nil {
		extra = make(map[string]any)
	}
	for key, value := range current {
		if strings.HasPrefix(key, "openai_rescue_") {
			extra[key] = value
		}
	}
	return extra
}
