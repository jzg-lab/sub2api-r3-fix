package repository

import (
	"context"
	"encoding/json"
	"slices"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

var _ service.AccountGroupEditRepository = (*accountRepository)(nil)

func (r *accountRepository) UpdateWithAccountGroups(ctx context.Context, account *service.Account, groupIDs []int64, probeEnabled, rateSyncEnabled *bool, rateMultiplier *float64, concurrency *int) error {
	return r.updateAccount(ctx, account, probeEnabled, rateSyncEnabled, rateMultiplier, &groupIDs, true, concurrency)
}

func (r *accountRepository) BulkUpdateWithAccountGroups(ctx context.Context, ids []int64, updates service.AccountBulkUpdate, groupIDs []int64) (int64, error) {
	ids = uniquePositiveInt64s(ids)
	slices.Sort(ids)
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	for _, id := range ids {
		if err := validateAccountRescueGroupEdit(txCtx, tx.Client(), id, groupIDs); err != nil {
			return 0, err
		}
	}
	if _, err := r.BulkUpdate(txCtx, ids, updates); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := replaceAccountGroupsInTx(txCtx, tx.Client(), id, groupIDs); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		r.syncSchedulerAccountSnapshotDetached(ctx, id)
	}
	return int64(len(ids)), nil
}

func validateAccountRescueGroupEdit(ctx context.Context, client *dbent.Client, id int64, groupIDs []int64) error {
	var markerJSON []byte
	if err := scanSingleRow(ctx, client, `SELECT extra -> 'openai_rescue_lane'
		FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, []any{id}, &markerJSON); err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	if len(markerJSON) == 0 || string(markerJSON) == "null" {
		return nil
	}
	var marker any
	if err := json.Unmarshal(markerJSON, &marker); err != nil {
		return err
	}
	groups, err := client.AccountGroup.Query().Where(dbaccountgroup.AccountIDEQ(id)).All(ctx)
	if err != nil {
		return err
	}
	account := &service.Account{Extra: map[string]any{"openai_rescue_lane": marker}}
	for _, group := range groups {
		account.GroupIDs = append(account.GroupIDs, group.GroupID)
	}
	return service.ValidateOpenAIRescueGroupEdit(account, groupIDs)
}

func replaceAccountGroupsInTx(ctx context.Context, client *dbent.Client, id int64, groupIDs []int64) error {
	if len(groupIDs) > 0 {
		var found int
		if err := scanSingleRow(ctx, client, `SELECT COUNT(*) FROM (
			SELECT id FROM groups WHERE id = ANY($1) AND deleted_at IS NULL ORDER BY id FOR KEY SHARE
		) AS targets`, []any{pq.Array(groupIDs)}, &found); err != nil {
			return err
		}
		if found != len(groupIDs) {
			return service.ErrGroupNotFound
		}
	}
	groups, err := client.AccountGroup.Query().Where(dbaccountgroup.AccountIDEQ(id)).All(ctx)
	if err != nil {
		return err
	}
	oldIDs := make([]int64, 0, len(groups))
	for _, group := range groups {
		oldIDs = append(oldIDs, group.GroupID)
	}
	if _, err := client.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if len(groupIDs) > 0 {
		builders := make([]*dbent.AccountGroupCreate, 0, len(groupIDs))
		for i, groupID := range groupIDs {
			builders = append(builders, client.AccountGroup.Create().SetAccountID(id).SetGroupID(groupID).SetPriority(i+1))
		}
		if _, err := client.AccountGroup.CreateBulk(builders...).Save(ctx); err != nil {
			return err
		}
	}
	return enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountGroupsChanged, &id, nil, buildSchedulerGroupPayload(mergeGroupIDs(oldIDs, groupIDs)))
}
