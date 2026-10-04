package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

func prepareOpenAIOAuthAccountCreate(ctx context.Context, exec sqlExecutor, account *service.Account) error {
	if !service.IsOpenAIBrowserOAuthAccount(account) {
		return nil
	}
	identityKind, identityValue, ok := service.OpenAIOAuthStableIdentity(account)
	if !ok {
		return service.ErrOpenAIOAuthStableIdentityRequired
	}
	if _, err := exec.ExecContext(
		ctx,
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		"sub2api:openai-oauth:"+identityKind+":"+identityValue,
	); err != nil {
		return err
	}

	rows, err := exec.QueryContext(ctx, `
		SELECT credentials, extra, proxy_id, deleted_at
		FROM accounts
		WHERE platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
			AND lower(btrim(COALESCE(
				CASE WHEN $1 = 'chatgpt_account_id'
					THEN credentials ->> 'chatgpt_account_id'
					ELSE credentials ->> 'email'
				END,
				''
			))) = $2
		ORDER BY id
		FOR UPDATE
	`, identityKind, identityValue)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var historicalProxyID int64
	var historicalLoginIP string
	historyFound := false
	for rows.Next() {
		var (
			credentialsJSON []byte
			extraJSON       []byte
			proxyID         sql.NullInt64
			deletedAt       sql.NullTime
		)
		if err := rows.Scan(&credentialsJSON, &extraJSON, &proxyID, &deletedAt); err != nil {
			return err
		}
		historical, err := openAIOAuthAccountFromJSON(credentialsJSON, extraJSON, proxyID)
		if err != nil {
			return err
		}
		if !service.IsOpenAIBrowserOAuthAccount(historical) {
			continue
		}
		kind, value, identityOK := service.OpenAIOAuthStableIdentity(historical)
		if !identityOK || kind != identityKind || value != identityValue {
			continue
		}
		if !deletedAt.Valid {
			return service.ErrOpenAIOAuthIdentityExists
		}
		raw, exists := historical.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]
		if !exists {
			return service.ErrOpenAIOAuthHistoryBindingMissing
		}
		qualifiedProxyID, bindingOK := service.OpenAIOAuthQualifiedProxyID(historical.Extra)
		if !bindingOK {
			return service.ErrOpenAIOAuthProxyBindingCorrupt
		}
		if !historyFound {
			historicalProxyID = qualifiedProxyID
			historyFound = true
		} else if historicalProxyID != qualifiedProxyID {
			return service.ErrOpenAIOAuthHistoryBindingConflict
		}
		if _, exists := historical.Extra[service.OpenAIOAuthLoginExitIPExtraKey]; exists {
			ip, err := service.OpenAIOAuthLoginExitIP(historical)
			if err != nil {
				return err
			}
			if historicalLoginIP != "" && historicalLoginIP != ip {
				return service.ErrOpenAIOAuthHistoryBindingConflict
			}
			historicalLoginIP = ip
		}
		_ = raw
	}
	if err := rows.Err(); err != nil {
		return err
	}
	loginIP, err := service.OpenAIOAuthInitialLoginIPForCreate(ctx, account, historyFound, historicalLoginIP)
	if err != nil {
		return err
	}

	if historyFound {
		if account.ProxyID != nil && *account.ProxyID != historicalProxyID {
			return service.ErrOpenAIOAuthProxyMismatch
		}
		account.ProxyID = int64Ptr(historicalProxyID)
	}
	account.Extra = maps.Clone(account.Extra)
	if account.Extra == nil {
		account.Extra = make(map[string]any, 3)
	}
	delete(account.Extra, service.OpenAIOAuthQualifiedProxyExtraKey)
	delete(account.Extra, service.OpenAIOAuthLoginExitIPExtraKey)
	if historicalLoginIP != "" {
		account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = historicalLoginIP
	} else if loginIP != "" {
		account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = loginIP
	}
	if account.ProxyID == nil || *account.ProxyID <= 0 {
		// A genuinely new identity without an authorization-time proxy may
		// come from the credential-import workflow. Preserve r17k's automatic
		// qualification/bucket assignment for that path. Deleted identities
		// never reach this branch unless their historical binding was valid.
		account.Extra[service.OpenAIDowngradeQualificationExtraKey] = true
		account.Schedulable = false
		return nil
	}
	if err := lockValidOpenAIOAuthProxy(ctx, exec, *account.ProxyID); err != nil {
		return err
	}
	if loginIP != "" {
		// The share lock above fences configuration edits until this create
		// commits; compare the full route, not just its numeric ID.
		proxy := &service.Proxy{ID: *account.ProxyID, Status: service.StatusActive}
		if err := scanSingleRow(ctx, exec, `
			SELECT protocol, host, port, COALESCE(username, ''), COALESCE(password, '')
			FROM proxies WHERE id = $1 AND deleted_at IS NULL
		`, []any{proxy.ID}, &proxy.Protocol, &proxy.Host, &proxy.Port, &proxy.Username, &proxy.Password); err != nil {
			return err
		}
		if err := service.ValidateOpenAIOAuthInitialLoginProxy(ctx, proxy); err != nil {
			return err
		}
	}

	if historyFound {
		account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] = historicalProxyID
	}
	account.Extra[service.OpenAIDowngradeQualificationExtraKey] = true
	account.Schedulable = false
	return nil
}

func validateOpenAIOAuthAccountUpdate(
	ctx context.Context,
	exec sqlExecutor,
	next *service.Account,
) error {
	if next == nil {
		return service.ErrAccountNilInput
	}
	var current *service.Account
	var err error
	if service.IsOpenAIBrowserOAuthAccount(next) {
		current, err = lockOAuthReauthorizationAccount(ctx, exec, next.ID)
	} else {
		current, err = lockOpenAIOAuthAccount(ctx, exec, next.ID)
	}
	if err != nil {
		return err
	}
	if err := validateOpenAIOAuthAccountReplacement(current, next); err != nil {
		return err
	}
	if service.IsOpenAIBrowserOAuthAccount(current) {
		// Admin validation happens before this transaction. A full-row edit
		// must not restore old tokens after a concurrent refresh or callback.
		if next.UpdatedAt.IsZero() || !current.UpdatedAt.Equal(next.UpdatedAt) {
			return service.ErrOAuthReauthorizationStale
		}
		next.Extra = maps.Clone(next.Extra)
		if next.Extra == nil {
			next.Extra = make(map[string]any)
		}
		delete(next.Extra, service.OpenAIOAuthQualifiedProxyExtraKey)
		delete(next.Extra, service.OpenAIOAuthLoginExitIPExtraKey)
		if originalIP, exists := current.Extra[service.OpenAIOAuthLoginExitIPExtraKey]; exists {
			next.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = originalIP
		}
		if raw, exists := current.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]; exists {
			if _, ok := service.OpenAIOAuthQualifiedProxyID(current.Extra); !ok {
				return service.ErrOpenAIOAuthProxyBindingCorrupt
			}
			next.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] = raw
		}
		if next.Schedulable {
			if err := validateOpenAIOAuthQualification(ctx, exec, next); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIOAuthCredentialsUpdate(
	ctx context.Context,
	exec sqlExecutor,
	accountID int64,
	credentials map[string]any,
) error {
	current, err := lockOpenAIOAuthAccount(ctx, exec, accountID)
	if err != nil {
		return err
	}
	if err := service.ValidateOpenAIOAuthCredentialSnapshot(ctx, current); err != nil {
		return err
	}
	next := *current
	next.Credentials = maps.Clone(credentials)
	return validateOpenAIOAuthAccountReplacement(current, &next)
}

func validateOpenAIOAuthBulkUpdate(
	ctx context.Context,
	exec sqlExecutor,
	ids []int64,
	updates service.AccountBulkUpdate,
) error {
	if !requiresOpenAIOAuthBulkValidation(updates) {
		return nil
	}
	rows, err := exec.QueryContext(ctx, `
		SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id
		FROM accounts
		WHERE id = ANY($1) AND deleted_at IS NULL
		ORDER BY id
		FOR NO KEY UPDATE
	`, pq.Array(ids))
	if err != nil {
		return err
	}

	accounts := make([]*service.Account, 0, len(ids))
	for rows.Next() {
		current, err := scanOpenAIOAuthAccount(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		accounts = append(accounts, current)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// PostgreSQL row locks remain held by the transaction after Rows is closed.
	// Close the result set before qualification performs another query on the
	// same transaction connection; lib/pq cannot run a nested query while Rows
	// is still consuming the wire protocol.
	for _, current := range accounts {
		next := *current
		next.Credentials = maps.Clone(current.Credentials)
		next.Extra = maps.Clone(current.Extra)
		if updates.ProxyID != nil {
			if *updates.ProxyID == 0 {
				next.ProxyID = nil
			} else {
				next.ProxyID = int64Ptr(*updates.ProxyID)
			}
		}
		for key, value := range updates.Credentials {
			if next.Credentials == nil {
				next.Credentials = make(map[string]any)
			}
			next.Credentials[key] = value
		}
		if err := validateOpenAIOAuthAccountReplacement(current, &next); err != nil {
			return err
		}
		if updates.Schedulable != nil && *updates.Schedulable &&
			service.IsOpenAIBrowserOAuthAccount(current) {
			if err := validateOpenAIOAuthQualification(ctx, exec, current); err != nil {
				return err
			}
		}
	}
	return nil
}

func requiresOpenAIOAuthBulkValidation(updates service.AccountBulkUpdate) bool {
	if updates.ProxyID != nil || (updates.Schedulable != nil && *updates.Schedulable) {
		return true
	}
	for key := range updates.Credentials {
		switch key {
		case "chatgpt_account_id", "email", "auth_mode", "openai_auth_mode":
			return true
		}
	}
	return false
}

func validateOpenAIOAuthSchedulable(
	ctx context.Context,
	exec sqlExecutor,
	accountID int64,
	schedulable bool,
) error {
	if !schedulable {
		return nil
	}
	account, err := lockOpenAIOAuthAccount(ctx, exec, accountID)
	if err != nil {
		return err
	}
	if !service.IsOpenAIBrowserOAuthAccount(account) {
		return nil
	}
	return validateOpenAIOAuthQualification(ctx, exec, account)
}

// accountBoundOnlyToGroup 账号当前是否仅绑定指定组。救治区放行的安全依据：
// 仅绑救治组 = 账号全部流量都从无客户订阅的救治组走，资格闸保护的客户面
// 不可能被触达；还绑着任何别的组（客户池）就不放行。
func accountBoundOnlyToGroup(ctx context.Context, exec sqlExecutor, accountID int64, groupID int64) (bool, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT group_id FROM account_groups WHERE account_id = $1
	`, accountID)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	only := false
	for rows.Next() {
		var bound int64
		if err := rows.Scan(&bound); err != nil {
			return false, err
		}
		if bound != groupID {
			return false, nil
		}
		only = true
	}
	return only, rows.Err()
}

// validateOpenAIOAuthSchedulableInRescueLane 救治区专用调度校验（r17ax）：
// 判死号入区要开调度，但救治区救的正是没过资格考（Extra 无合格戳）的号，
// 全局资格闸会 409。放行条件收紧为「账号当前仅绑定救治组」——闸的语义
// （未考证 OAuth 号不得进客户流量）在救治组拓扑下不可能被违反；代理在场
// 校验保留（无代理种子/插件都无从工作）。
func validateOpenAIOAuthSchedulableInRescueLane(
	ctx context.Context,
	exec sqlExecutor,
	accountID int64,
	rescueGroupID int64,
	schedulable bool,
) error {
	if !schedulable {
		return nil
	}
	account, err := lockOpenAIOAuthAccount(ctx, exec, accountID)
	if err != nil {
		return err
	}
	if !service.IsOpenAIBrowserOAuthAccount(account) {
		return nil
	}
	if rescueGroupID <= 0 {
		return service.ErrOpenAIOAuthRescueBindingRequired
	}
	only, err := accountBoundOnlyToGroup(ctx, exec, accountID, rescueGroupID)
	if err != nil {
		return err
	}
	if !only {
		return service.ErrOpenAIOAuthRescueBindingRequired
	}
	if account.ProxyID == nil || *account.ProxyID <= 0 {
		return service.ErrOpenAIOAuthProxyRequired
	}
	return nil
}

func validateOpenAIOAuthQualification(ctx context.Context, exec sqlExecutor, account *service.Account) error {
	if account == nil || account.ProxyID == nil || *account.ProxyID <= 0 {
		return service.ErrOpenAIOAuthProxyRequired
	}
	qualifiedProxyID, ok := service.OpenAIOAuthQualifiedProxyID(account.Extra)
	if !ok {
		if _, exists := account.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]; exists {
			return service.ErrOpenAIOAuthProxyBindingCorrupt
		}
		return service.ErrOpenAIOAuthQualificationRequired
	}
	if qualifiedProxyID != *account.ProxyID {
		return service.ErrOpenAIOAuthProxyMismatch
	}
	return validateOpenAIOAuthProxy(ctx, exec, qualifiedProxyID)
}

func validateOpenAIOAuthAccountReplacement(current, next *service.Account) error {
	currentProtected := service.IsOpenAIBrowserOAuthAccount(current)
	nextProtected := service.IsOpenAIBrowserOAuthAccount(next)
	if !currentProtected && !nextProtected {
		return nil
	}
	if currentProtected != nextProtected {
		return service.ErrOpenAIOAuthIdentityChanged
	}
	currentKind, currentIdentity, currentOK := service.OpenAIOAuthStableIdentity(current)
	nextKind, nextIdentity, nextOK := service.OpenAIOAuthStableIdentity(next)
	if !currentOK || !nextOK || currentKind != nextKind || currentIdentity != nextIdentity {
		return service.ErrOpenAIOAuthIdentityChanged
	}
	if !sameNullableInt64(current.ProxyID, next.ProxyID) {
		// Fresh imports have no authorization binding yet and must be allowed
		// to receive their first qualified proxy. Once any proxy or qualified
		// binding exists, the authorization route is immutable.
		_, currentQualified := service.OpenAIOAuthQualifiedProxyID(current.Extra)
		if current.ProxyID != nil || currentQualified ||
			current.Extra[service.OpenAIOAuthQualifiedProxyExtraKey] != nil {
			return service.ErrOpenAIOAuthProxyBindingProtected
		}
	}
	if raw, exists := current.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]; exists {
		qualifiedProxyID, ok := service.OpenAIOAuthQualifiedProxyID(current.Extra)
		if !ok {
			return service.ErrOpenAIOAuthProxyBindingCorrupt
		}
		if current.ProxyID == nil || *current.ProxyID != qualifiedProxyID {
			return service.ErrOpenAIOAuthProxyMismatch
		}
		_ = raw
	}
	return nil
}

func lockOpenAIOAuthAccount(ctx context.Context, exec sqlExecutor, accountID int64) (*service.Account, error) {
	rows, err := exec.QueryContext(ctx, `
		SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		FOR NO KEY UPDATE
	`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAccountNotFound
	}
	account, err := scanOpenAIOAuthAccount(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return account, nil
}

type sqlRowScanner interface {
	Scan(...any) error
}

func scanOpenAIOAuthAccount(scanner sqlRowScanner) (*service.Account, error) {
	var (
		account         service.Account
		credentialsJSON []byte
		extraJSON       []byte
		proxyID         sql.NullInt64
		parentAccountID sql.NullInt64
	)
	if err := scanner.Scan(
		&account.ID,
		&account.Platform,
		&account.Type,
		&credentialsJSON,
		&extraJSON,
		&proxyID,
		&parentAccountID,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(credentialsJSON, &account.Credentials); err != nil {
		return nil, fmt.Errorf("decode account credentials: %w", err)
	}
	if len(extraJSON) > 0 && string(extraJSON) != "null" {
		if err := json.Unmarshal(extraJSON, &account.Extra); err != nil {
			return nil, fmt.Errorf("decode account extra: %w", err)
		}
	}
	if proxyID.Valid {
		account.ProxyID = int64Ptr(proxyID.Int64)
	}
	if parentAccountID.Valid {
		account.ParentAccountID = int64Ptr(parentAccountID.Int64)
	}
	return &account, nil
}

func openAIOAuthAccountFromJSON(credentialsJSON, extraJSON []byte, proxyID sql.NullInt64) (*service.Account, error) {
	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
	}
	if err := json.Unmarshal(credentialsJSON, &account.Credentials); err != nil {
		return nil, fmt.Errorf("decode historical account credentials: %w", err)
	}
	if len(extraJSON) > 0 && string(extraJSON) != "null" {
		if err := json.Unmarshal(extraJSON, &account.Extra); err != nil {
			return nil, fmt.Errorf("decode historical account extra: %w", err)
		}
	}
	if proxyID.Valid {
		account.ProxyID = int64Ptr(proxyID.Int64)
	}
	return account, nil
}

func validateOpenAIOAuthProxy(ctx context.Context, exec sqlExecutor, proxyID int64) error {
	var valid bool
	err := scanSingleRow(ctx, exec, `
		SELECT EXISTS (
			SELECT 1
			FROM proxies
			WHERE id = $1 AND deleted_at IS NULL AND status = $2
				AND (expires_at IS NULL OR expires_at > NOW())
		)
	`, []any{proxyID, service.StatusActive}, &valid)
	if err != nil {
		return err
	}
	if !valid {
		return service.ErrOpenAIOAuthProxyInvalid
	}
	return nil
}

func lockValidOpenAIOAuthProxy(ctx context.Context, exec sqlExecutor, proxyID int64) error {
	var (
		status    string
		expiresAt sql.NullTime
	)
	err := scanSingleRow(ctx, exec, `
		SELECT status, expires_at
		FROM proxies
		WHERE id = $1 AND deleted_at IS NULL
		FOR SHARE
	`, []any{proxyID}, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrOpenAIOAuthProxyInvalid
	}
	if err != nil {
		return err
	}
	if status != service.StatusActive || (expiresAt.Valid && !expiresAt.Time.After(time.Now())) {
		return service.ErrOpenAIOAuthProxyInvalid
	}
	return nil
}

func validateOpenAIOAuthProtectedProxyUpdate(
	ctx context.Context,
	exec sqlExecutor,
	proxyID int64,
	identityChanged bool,
	nextExpiresAt *time.Time,
) error {
	protected, err := openAIOAuthProxyIsProtected(ctx, exec, proxyID)
	if err != nil || !protected {
		return err
	}
	var currentExpiresAt sql.NullTime
	if err := scanSingleRow(ctx, exec, `
		SELECT expires_at
		FROM proxies
		WHERE id = $1 AND deleted_at IS NULL
	`, []any{proxyID}, &currentExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrProxyNotFound
		}
		return err
	}
	if identityChanged || !sameNullableTime(currentExpiresAt, nextExpiresAt) {
		return service.ErrOpenAIOAuthProxyBindingProtected
	}
	return nil
}

func validateOpenAIOAuthProtectedProxyDelete(ctx context.Context, exec sqlExecutor, proxyID int64) error {
	protected, err := openAIOAuthProxyIsProtected(ctx, exec, proxyID)
	if err != nil {
		return err
	}
	if protected {
		return service.ErrOpenAIOAuthProxyBindingProtected
	}
	return nil
}

func openAIOAuthProxyIsProtected(ctx context.Context, exec sqlExecutor, proxyID int64) (bool, error) {
	var protected bool
	err := scanSingleRow(ctx, exec, `
		SELECT EXISTS (
			SELECT 1
			FROM accounts
			WHERE (`+openAIBrowserOAuthAccountSQL+`)
				AND (
					proxy_id = $1
					OR extra ->> '`+service.OpenAIOAuthQualifiedProxyExtraKey+`' = $2
				)
		)
	`, []any{proxyID, fmt.Sprint(proxyID)}, &protected)
	return protected, err
}

func sameNullableTime(current sql.NullTime, next *time.Time) bool {
	if !current.Valid || next == nil {
		return !current.Valid && next == nil
	}
	return current.Time.Equal(*next)
}

func sameNullableInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func int64Ptr(value int64) *int64 {
	return &value
}

func isOpenAIOAuthProxyBindingError(err error) bool {
	return errors.Is(err, service.ErrOpenAIOAuthProxyBindingProtected) ||
		errors.Is(err, service.ErrOpenAIOAuthIdentityChanged) ||
		errors.Is(err, service.ErrOpenAIOAuthProxyMismatch)
}

func validateOpenAIOAuthProxyMutation(
	ctx context.Context,
	exec sqlExecutor,
	accountID int64,
	proxyID *int64,
) error {
	current, err := lockOpenAIOAuthAccount(ctx, exec, accountID)
	if err != nil {
		return err
	}
	if !service.IsOpenAIBrowserOAuthAccount(current) {
		return nil
	}
	if !sameNullableInt64(current.ProxyID, proxyID) {
		return service.ErrOpenAIOAuthProxyBindingProtected
	}
	return nil
}

func validateOpenAIOAuthProxyFallbackRevert(
	ctx context.Context,
	exec sqlExecutor,
	accountID int64,
) error {
	current, err := lockOpenAIOAuthAccount(ctx, exec, accountID)
	if err != nil {
		return err
	}
	if !service.IsOpenAIBrowserOAuthAccount(current) {
		return nil
	}
	qualifiedProxyID, ok := service.OpenAIOAuthQualifiedProxyID(current.Extra)
	if !ok {
		if _, exists := current.Extra[service.OpenAIOAuthQualifiedProxyExtraKey]; exists {
			return service.ErrOpenAIOAuthProxyBindingCorrupt
		}
		return service.ErrOpenAIOAuthQualificationRequired
	}
	var originProxyID sql.NullInt64
	if err := scanSingleRow(ctx, exec, `
		SELECT proxy_fallback_origin_id
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
	`, []any{accountID}, &originProxyID); err != nil {
		return err
	}
	if !originProxyID.Valid {
		return service.ErrAccountNotInFallback
	}
	if originProxyID.Int64 != qualifiedProxyID {
		return service.ErrOpenAIOAuthProxyBindingProtected
	}
	return validateOpenAIOAuthProxy(ctx, exec, qualifiedProxyID)
}
