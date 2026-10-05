package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const OpenAIRescueTerminatedAtExtraKey = "openai_rescue_terminated_at"

var ErrOpenAIRescueTerminated = infraerrors.Conflict("OPENAI_RESCUE_TERMINATED", "automatic rescue is stopped; restart rescue manually")

// Entry and termination share the account lock with group edits and probe commits.
type OpenAIRescueLifecycleRepository interface {
	EnterOpenAIRescue(context.Context, *Account, map[string]any, int64, bool) (bool, error)
	TerminateOpenAIRescue(context.Context, int64, time.Time) (bool, error)
	MutateOpenAIRescue(context.Context, int64, any, *[]int64, map[string]any, bool) (bool, error)
}

func OpenAIRescueManuallyTerminated(account *Account) bool {
	return account != nil && account.Extra[OpenAIRescueTerminatedAtExtraKey] != nil
}

func (l *OpenAIRescueLane) repairRescueGroups(ctx context.Context, account *Account, groupID int64) error {
	groupIDs := []int64{groupID}
	if repo, ok := l.accounts.(OpenAIRescueLifecycleRepository); ok {
		_, err := repo.MutateOpenAIRescue(ctx, account.ID, account.Extra[openAIRescueLaneExtraKey], &groupIDs, nil, false)
		return err
	}
	return l.accounts.BindGroups(ctx, account.ID, groupIDs)
}

func OpenAIRescueOriginalGroups(raw any) ([]int64, error) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("rescue original group snapshot is invalid")
	}
	originalGroups, present := fields["orig_group_ids"]
	if !present {
		return nil, errors.New("rescue original group snapshot is missing")
	}
	groupJSON, err := json.Marshal(originalGroups)
	if err != nil {
		return nil, fmt.Errorf("rescue original group snapshot: %w", err)
	}
	var groupIDs []int64
	if err := json.Unmarshal(groupJSON, &groupIDs); err != nil {
		return nil, fmt.Errorf("rescue original group snapshot: %w", err)
	}
	if groupIDs == nil {
		return nil, errors.New("rescue original group snapshot must be an array")
	}
	seen := make(map[int64]bool, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 || seen[id] {
			return nil, errors.New("rescue original group snapshot contains an invalid or duplicate group ID")
		}
		seen[id] = true
	}
	return groupIDs, nil
}

func (l *OpenAIRescueLane) TerminateRescue(ctx context.Context, accountID int64) (bool, error) {
	if l == nil {
		return false, errors.New("rescue lane is not available")
	}
	repo, ok := l.accounts.(OpenAIRescueLifecycleRepository)
	if !ok {
		return false, ErrOpenAIProbeAtomicStore
	}
	terminated, err := repo.TerminateOpenAIRescue(ctx, accountID, l.now().UTC())
	if err == nil {
		if syncErr := l.syncPluginProbePauses(ctx); syncErr != nil {
			// The account transaction is durable; reconcile retries on every instance.
			slog.Warn("openai_rescue_plugin_pause_sync_failed", "account_id", accountID, "error", syncErr)
		}
	}
	return terminated, err
}

func (r *OpenAIDowngradeProbeRunner) TerminateRescueAccount(ctx context.Context, accountID int64) (map[string]any, error) {
	if r == nil || r.rescueLane == nil {
		return nil, errors.New("rescue lane is not available")
	}
	terminated, err := r.rescueLane.TerminateRescue(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"account_id": accountID, "terminated": terminated}, nil
}
