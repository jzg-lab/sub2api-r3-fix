package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// ApplyOAuthCredentials replaces OAuth credentials and clears recoverable
// runtime state in one transaction. The row lock protects the authorization
// identity check and the subsequent replacement from concurrent credential edits.
func (r *accountRepository) ApplyOAuthCredentials(
	ctx context.Context,
	id int64,
	expectedUpdatedAt time.Time,
	accountType string,
	credentials map[string]any,
	extraUpdates map[string]any,
) (*service.Account, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("account repository is not initialized")
	}
	if expectedUpdatedAt.IsZero() {
		return nil, service.ErrOAuthReauthorizationStale
	}

	if accountType != service.AccountTypeOAuth && accountType != service.AccountTypeSetupToken {
		return nil, infraerrors.BadRequest("NOT_OAUTH", "invalid OAuth account type")
	}
	if token, ok := credentials["access_token"].(string); !ok || strings.TrimSpace(token) == "" {
		return nil, infraerrors.BadRequest("OAUTH_CREDENTIALS_REQUIRED", "OAuth access token is required")
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	txExec, ok := any(client.Driver()).(sqlExecutor)
	if !ok {
		return nil, errors.New("transaction driver does not support context SQL execution")
	}

	current, err := lockOAuthReauthorizationAccount(txCtx, txExec, id)
	if err != nil {
		return nil, err
	}
	if !current.IsOAuth() || current.IsCredentialShadow() {
		return nil, infraerrors.BadRequest("NOT_OAUTH", "account does not own OAuth credentials")
	}
	if current.Platform != service.PlatformAnthropic && accountType != service.AccountTypeOAuth {
		return nil, infraerrors.BadRequest("NOT_OAUTH", "setup-token is only supported for Anthropic")
	}
	if err := service.ValidateOAuthReauthorizationUpdate(txCtx, current, expectedUpdatedAt, credentials); err != nil {
		return nil, err
	}

	mergedExtra := maps.Clone(current.Extra)
	if mergedExtra == nil {
		mergedExtra = make(map[string]any)
	}
	for key, value := range extraUpdates {
		if strings.HasPrefix(key, "openai_rescue_") {
			continue
		}
		switch key {
		case "codex_fingerprint_seed", service.OpenAIOAuthQualifiedProxyExtraKey,
			service.OpenAIDowngradeSolFallbackExtraKey, service.OpenAIDowngradeQualificationExtraKey:
			continue
		}
		mergedExtra[key] = value
	}
	// Clear derived failures after merging, so a stale form cannot restore them.
	delete(mergedExtra, "antigravity_quota_scopes")
	delete(mergedExtra, "model_rate_limits")
	if current.Platform == service.PlatformGrok {
		mergedExtra["grok_needs_reauth"] = false
		mergedExtra["grok_needs_reauth_reason"] = ""
		mergedExtra["grok_needs_reauth_at"] = ""
	}

	next := *current
	next.Type = accountType
	next.Credentials = mergeReauthorizedCredentials(current.Credentials, credentials)
	next.Extra = mergedExtra

	if current.IsOpenAIOAuth() {
		if err := validateOpenAIOAuthAccountReplacement(current, &next); err != nil {
			return nil, err
		}
	}

	openAIQualified := true
	if service.IsOpenAIBrowserOAuthAccount(&next) {
		if err := validateOpenAIOAuthQualification(txCtx, client, &next); err != nil {
			// Missing qualification may be repaired later; database and corrupt
			// binding errors must roll back rather than masquerade as success.
			if !errors.Is(err, service.ErrOpenAIOAuthQualificationRequired) &&
				!errors.Is(err, service.ErrOpenAIOAuthProxyRequired) &&
				!errors.Is(err, service.ErrOpenAIOAuthProxyInvalid) {
				return nil, err
			}
			openAIQualified = false
		}
	}

	credentialJSON, err := json.Marshal(normalizeJSONMap(next.Credentials))
	if err != nil {
		return nil, err
	}
	extraJSON, err := json.Marshal(normalizeJSONMap(mergedExtra))
	if err != nil {
		return nil, err
	}

	status := current.Status
	schedulable := current.Schedulable
	if current.Status == service.StatusError {
		status = service.StatusActive
		schedulable = true
	}
	if service.IsOpenAIBrowserOAuthAccount(&next) && !openAIQualified {
		schedulable = false
	}
	// Replacing credentials is not the rescue lane's qualification verdict.
	if current.IsOpenAIOAuth() &&
		(service.GetOpenAIRescueLaneMarker(current) != nil || service.GetOpenAIRescueSuspected(current)) {
		schedulable = false
	}

	result, err := txExec.ExecContext(txCtx, `
		UPDATE accounts
		SET type = $1,
			credentials = $2::jsonb,
			extra = $3::jsonb,
			status = $4,
			error_message = '',
			schedulable = $5,
			rate_limited_at = NULL,
			rate_limit_reset_at = NULL,
			overload_until = NULL,
			temp_unschedulable_until = NULL,
			temp_unschedulable_reason = NULL,
			updated_at = NOW()
		WHERE id = $6
			AND deleted_at IS NULL
			AND updated_at = $7
	`, accountType, string(credentialJSON), string(extraJSON), status, schedulable, id, current.UpdatedAt)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrOAuthReauthorizationStale
	}
	if err := enqueueSchedulerOutbox(txCtx, txExec, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return nil, err
	}
	// Read our own write before committing. A canceled client or a subsequent
	// edit must not turn a committed replacement into a failed/mixed response.
	txRepo := newAccountRepositoryWithSQL(client, txExec, nil)
	updated, err := txRepo.GetByID(txCtx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return updated, nil
}

func lockOAuthReauthorizationAccount(
	ctx context.Context,
	exec sqlExecutor,
	id int64,
) (*service.Account, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id,
		       status, schedulable, updated_at
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		FOR NO KEY UPDATE
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		account     service.Account
		credentials []byte
		extra       []byte
		proxyID     sql.NullInt64
		parentID    sql.NullInt64
	)
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAccountNotFound
	}
	if err := rows.Scan(
		&account.ID,
		&account.Platform,
		&account.Type,
		&credentials,
		&extra,
		&proxyID,
		&parentID,
		&account.Status,
		&account.Schedulable,
		&account.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if proxyID.Valid {
		account.ProxyID = &proxyID.Int64
	}
	if parentID.Valid {
		account.ParentAccountID = &parentID.Int64
	}
	if len(credentials) > 0 && string(credentials) != "null" {
		if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
			return nil, err
		}
	}
	if len(extra) > 0 && string(extra) != "null" {
		if err := json.Unmarshal(extra, &account.Extra); err != nil {
			return nil, err
		}
	}
	return &account, nil
}

func mergeReauthorizedCredentials(current, incoming map[string]any) map[string]any {
	merged := maps.Clone(current)
	if merged == nil {
		merged = make(map[string]any)
	}
	// Replace the credential generation, not routing configuration. In
	// particular, absent model_mapping must not widen an account's model set.
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "expires_in", "token_type", "scope"} {
		delete(merged, key)
	}
	maps.Copy(merged, incoming)
	service.SanitizeStoredCredentials("", merged)
	previous := (&service.Account{Credentials: current}).GetCredentialAsInt64("_token_version")
	version := time.Now().UnixMilli()
	if version <= previous {
		version = previous + 1
	}
	merged["_token_version"] = version
	return merged
}
