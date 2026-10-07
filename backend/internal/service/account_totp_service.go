package service

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrAccountTOTPInvalid     = infraerrors.BadRequest("ACCOUNT_TOTP_INVALID", "provide a valid Base32 TOTP secret or an explicit clear, with the current authorization revision")
	ErrAccountTOTPKey         = infraerrors.ServiceUnavailable("ACCOUNT_TOTP_KEY_REQUIRED", "a persistent TOTP encryption key must be configured")
	ErrAccountTOTPUnavailable = infraerrors.Conflict("ACCOUNT_TOTP_UNAVAILABLE", "stored TOTP is missing, cannot be decrypted, or belongs to a different account identity; import it again")
)

// AccountTOTPService owns the private SQL column; generic Ent account writes
// never read, export or replace it. Passwords are not persisted.
type AccountTOTPService struct {
	client        *dbent.Client
	encryptor     SecretEncryptor
	keyConfigured bool
}

func NewAccountTOTPService(client *dbent.Client, encryptor SecretEncryptor, cfg *config.Config) *AccountTOTPService {
	return &AccountTOTPService{client: client, encryptor: encryptor, keyConfigured: cfg != nil && cfg.Totp.EncryptionKeyConfigured}
}

type AccountTOTPInput struct {
	ExpectedRevision string  `json:"expected_authorization_revision"`
	TOTPSecret       *string `json:"totp_secret,omitempty"`
	MFASecret        *string `json:"mfa_secret,omitempty"`
	Clear            bool    `json:"clear"`
}

type AccountTOTPStatus struct {
	HasSecret     bool `json:"has_totp_secret"`
	KeyConfigured bool `json:"encryption_key_configured"`
}

func NormalizeAccountTOTP(raw string) (string, error) {
	if len(raw) > 256 {
		return "", ErrAccountTOTPInvalid
	}
	value := strings.ToUpper(strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "-", "").Replace(raw))
	value = strings.TrimRight(value, "=")
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(value)
	if err != nil || len(decoded) < 10 || len(decoded) > 128 || base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded) != value {
		return "", ErrAccountTOTPInvalid
	}
	return value, nil
}

func (in AccountTOTPInput) secret() (*string, error) {
	if !validOpenAIOAuthAccountRevision(in.ExpectedRevision) || (in.Clear && (in.TOTPSecret != nil || in.MFASecret != nil)) {
		return nil, ErrAccountTOTPInvalid
	}
	var value *string
	for _, raw := range []*string{in.TOTPSecret, in.MFASecret} {
		if raw == nil {
			continue
		}
		normalized, err := NormalizeAccountTOTP(*raw)
		if err != nil {
			return nil, err
		}
		if value != nil && *value != normalized {
			return nil, ErrAccountTOTPInvalid
		}
		value = &normalized
	}
	return value, nil
}

func totpAccount(row *dbent.Account) *Account {
	return &Account{ID: row.ID, Platform: row.Platform, Type: row.Type, ParentAccountID: row.ParentAccountID, ProxyID: row.ProxyID, Credentials: row.Credentials, Extra: row.Extra}
}

// Bind the encrypted payload to durable login identity, independent of token
// rotation, scheduling, proxy selection and runtime health.
type accountTOTPPayload struct {
	AccountID int64  `json:"account_id"`
	Email     string `json:"email"`
	UserID    string `json:"user_id"`
	Secret    string `json:"secret"`
}

func accountTOTPIdentity(a *Account) accountTOTPPayload {
	return accountTOTPPayload{AccountID: a.ID, Email: strings.ToLower(strings.TrimSpace(a.GetCredential("email"))), UserID: a.GetCredential("chatgpt_user_id")}
}

func (s *AccountTOTPService) Status(ctx context.Context, id int64) (*AccountTOTPStatus, error) {
	rows, err := s.client.QueryContext(ctx, "SELECT totp_secret_encrypted IS NOT NULL FROM accounts WHERE id=$1 AND deleted_at IS NULL", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil {
			return nil, rows.Err()
		}
		return nil, ErrAccountNotFound
	}
	status := &AccountTOTPStatus{KeyConfigured: s.keyConfigured}
	if err := rows.Scan(&status.HasSecret); err != nil {
		return nil, err
	}
	return status, nil
}

func (s *AccountTOTPService) Save(ctx context.Context, id int64, in AccountTOTPInput) (*AccountTOTPStatus, error) {
	value, err := in.secret()
	if err != nil {
		return nil, err
	}
	if value != nil && !s.keyConfigured {
		return nil, ErrAccountTOTPKey
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row, err := tx.Account.Query().Where(dbaccount.IDEQ(id)).ForUpdate().Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, ErrAccountNotFound
	}
	if err != nil {
		return nil, err
	}
	a := totpAccount(row)
	if !IsOpenAIBrowserOAuthAccount(a) {
		return nil, ErrOpenAIOAuthReauthorizationUnsupported
	}
	if OpenAIOAuthAccountRevision(a) != in.ExpectedRevision {
		return nil, ErrOAuthReauthorizationStale
	}
	if in.Clear || value != nil {
		var encrypted any
		if value != nil {
			payload := accountTOTPIdentity(a)
			if payload.Email == "" {
				return nil, ErrAccountTOTPInvalid
			}
			payload.Secret = *value
			raw, _ := json.Marshal(payload)
			encrypted, err = s.encryptor.Encrypt(string(raw))
			clear(raw)
			if err != nil {
				return nil, ErrAccountTOTPUnavailable
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE accounts SET totp_secret_encrypted=$1 WHERE id=$2", encrypted, id); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return s.Status(ctx, id)
}

// Load is only used server-side for an explicitly requested, account-bound login.
func (s *AccountTOTPService) Load(ctx context.Context, session *OpenAIOAuthSession, email string) (string, error) {
	if !s.keyConfigured {
		return "", ErrAccountTOTPKey
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	row, err := tx.Account.Query().Where(dbaccount.IDEQ(session.ReauthorizationAccountID)).ForUpdate().Only(ctx)
	if err != nil {
		return "", ErrAccountTOTPUnavailable
	}
	a := totpAccount(row)
	if !IsOpenAIBrowserOAuthAccount(a) || OpenAIOAuthAccountRevision(a) != session.ReauthorizationAccountRevision {
		return "", ErrOAuthReauthorizationStale
	}
	rows, err := tx.QueryContext(ctx, "SELECT totp_secret_encrypted FROM accounts WHERE id=$1", a.ID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ciphertext sql.NullString
	if !rows.Next() {
		return "", ErrAccountTOTPUnavailable
	}
	if err := rows.Scan(&ciphertext); err != nil {
		return "", err
	}
	if !ciphertext.Valid {
		return "", ErrAccountTOTPUnavailable
	}
	plaintext, err := s.encryptor.Decrypt(ciphertext.String)
	if err != nil {
		return "", ErrAccountTOTPUnavailable
	}
	var payload accountTOTPPayload
	if json.Unmarshal([]byte(plaintext), &payload) != nil {
		return "", ErrAccountTOTPUnavailable
	}
	identity := accountTOTPIdentity(a)
	if payload.AccountID != identity.AccountID || payload.Email != identity.Email || payload.UserID != identity.UserID || payload.Email != strings.ToLower(strings.TrimSpace(email)) {
		return "", ErrAccountTOTPUnavailable
	}
	secret, err := NormalizeAccountTOTP(payload.Secret)
	if err != nil {
		return "", ErrAccountTOTPUnavailable
	}
	return secret, nil
}
