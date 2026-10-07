package service

import (
	"context"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var ErrOpenAIRescueGroupLocked = infraerrors.Conflict("OPENAI_RESCUE_GROUP_LOCKED", "account groups cannot be changed during rescue; finish or terminate rescue first")

// AccountGroupEditRepository checks rescue ownership and commits the whole edit
// with its group bindings, so a concurrent rescue cannot cause a partial save.
type AccountGroupEditRepository interface {
	UpdateWithAccountGroups(context.Context, *Account, []int64, *bool, *bool, *float64, *int) error
	BulkUpdateWithAccountGroups(context.Context, []int64, AccountBulkUpdate, []int64) (int64, error)
}

func ValidateOpenAIRescueGroupEdit(account *Account, groupIDs []int64) error {
	if account == nil || account.Extra[openAIRescueLaneExtraKey] == nil {
		return nil
	}
	if len(account.GroupIDs) != len(groupIDs) {
		return ErrOpenAIRescueGroupLocked
	}
	current := make(map[int64]bool, len(account.GroupIDs))
	for _, id := range account.GroupIDs {
		current[id] = true
	}
	for _, id := range groupIDs {
		if !current[id] {
			return ErrOpenAIRescueGroupLocked
		}
		delete(current, id)
	}
	return nil
}
