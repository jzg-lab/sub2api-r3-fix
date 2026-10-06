package repository

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Call under the account transaction lock so group edits cannot race the decision.
func openAIQualityProtectionApplies(ctx context.Context, exec sqlExecutor, accountID int64) (bool, error) {
	var raw string
	if err := scanSingleRow(ctx, exec, `SELECT COALESCE(
		(SELECT value FROM settings WHERE key = $1), '{}')`,
		[]any{service.SettingKeyOpenAIOperations}, &raw); err != nil {
		return true, err
	}
	settings := service.DefaultOpenAIOperationsSettings()
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return true, err
	}
	if err := settings.Validate(); err != nil {
		return true, err
	}
	if settings.QualityProtectedGroupIDs == nil {
		return true, nil
	}
	var markerJSON, groupsJSON []byte
	if err := scanSingleRow(ctx, exec, `SELECT extra -> 'openai_rescue_lane',
		COALESCE((SELECT jsonb_agg(group_id ORDER BY group_id) FROM account_groups WHERE account_id = accounts.id), '[]'::jsonb)
		FROM accounts WHERE id = $1 AND deleted_at IS NULL`, []any{accountID}, &markerJSON, &groupsJSON); err != nil {
		return true, err
	}
	var groupIDs []int64
	if len(markerJSON) > 0 && string(markerJSON) != "null" {
		var marker any
		if err := json.Unmarshal(markerJSON, &marker); err != nil {
			return true, err
		}
		var err error
		groupIDs, err = service.OpenAIRescueOriginalGroups(marker)
		if err != nil {
			return true, err
		}
	} else if err := json.Unmarshal(groupsJSON, &groupIDs); err != nil {
		return true, err
	}
	for _, id := range groupIDs {
		if !slices.Contains(settings.QualityProtectedGroupIDs, id) {
			return false, nil
		}
	}
	return len(groupIDs) > 0, nil
}
