package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

var ErrOpenAIOAuthReauthorizationProofRequired = infraerrors.Conflict(
	"OPENAI_OAUTH_REAUTH_PROOF_REQUIRED",
	"authorization result is missing, expired or does not match this account",
)

const openAIReauthorizationProofTTL = 5 * time.Minute

type openAIReauthorizationProofKey struct{}

var openAIReauthorizationCredentialKeys = [...]string{
	"access_token", "refresh_token", "id_token", "client_id", "email",
	"chatgpt_account_id", "chatgpt_user_id", "organization_id",
	"auth_mode", "openai_auth_mode",
}

// Hash only authorization material and identity. Frontends may represent expiry
// metadata differently, but may not substitute any token or account identity.
func openAIReauthorizationCredentialsHash(credentials map[string]any) (string, error) {
	values := make(map[string]string)
	for _, key := range openAIReauthorizationCredentialKeys {
		value, exists := credentials[key]
		if !exists {
			values[key] = ""
			continue
		}
		text, ok := value.(string)
		if !ok {
			return "", ErrOpenAIOAuthReauthorizationProofRequired
		}
		values[key] = text
	}
	if strings.TrimSpace(values["access_token"]) == "" {
		return "", ErrOpenAIOAuthReauthorizationProofRequired
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return "", ErrOpenAIOAuthReauthorizationProofRequired
	}
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:]), nil
}

func (s *OpenAIOAuthService) issueReauthorizationProof(
	ctx context.Context, session *OpenAIOAuthSession, result *OpenAITokenInfo,
) error {
	if session.ReauthorizationAccountID == 0 {
		return nil
	}
	hash, err := openAIReauthorizationCredentialsHash(s.BuildAccountCredentials(result))
	if err != nil {
		return err
	}
	id, err := openai.GenerateSessionID()
	if err != nil {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	proof := *session
	proof.ID = id
	proof.State = ""
	proof.CodeVerifier = ""
	proof.ReauthorizationBrowserSessionID = session.ID
	proof.CreatedAt = time.Now().UTC()
	proof.ReauthorizationCredentialsHash = hash
	if err := s.sessionStore.Create(ctx, &proof); err != nil {
		return err
	}
	result.ReauthorizationProof = id
	return nil
}

// Consume the durable, one-shot result before the transaction. A failed commit
// requires new authorization; neither a stale result nor an arbitrary import
// can be used to overwrite an account after an identity change.
func (s *OpenAIOAuthService) ConsumeReauthorizationProof(
	ctx context.Context, proofID string, accountID int64, revision time.Time, credentials map[string]any,
) (context.Context, error) {
	if s == nil || s.sessionStore == nil || strings.TrimSpace(proofID) == "" {
		return nil, ErrOpenAIOAuthReauthorizationProofRequired
	}
	proof, err := s.sessionStore.Get(ctx, proofID)
	if err != nil || proof == nil || proof.ID != proofID {
		return nil, ErrOpenAIOAuthReauthorizationProofRequired
	}
	if err := validateOpenAIOAuthReauthorizationBinding(proof); err != nil {
		return nil, ErrOpenAIOAuthReauthorizationProofRequired
	}
	hash, err := openAIReauthorizationCredentialsHash(credentials)
	now := time.Now()
	if err != nil || proof.ReauthorizationCredentialsHash != hash ||
		proof.ReauthorizationAccountID != accountID ||
		proof.ReauthorizationRevision != revision.UTC().Format(time.RFC3339Nano) ||
		proof.CreatedAt.After(now) || !now.Before(proof.CreatedAt.Add(openAIReauthorizationProofTTL)) {
		return nil, ErrOpenAIOAuthReauthorizationProofRequired
	}
	if err := s.validateReauthorizationSession(ctx, proof, true); err != nil {
		return nil, err
	}
	expected := *proof
	consumed, err := s.sessionStore.Consume(ctx, proofID)
	if err != nil || consumed == nil || *consumed != expected {
		return nil, ErrOpenAIOAuthReauthorizationProofRequired
	}
	if err := s.validateReauthorizationSession(ctx, consumed, false); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, openAIReauthorizationProofKey{}, expected), nil
}

// ValidateOAuthReauthorizationUpdate is used both before and under the row lock.
// Only a consumed, account-bound proof can replace the legacy timestamp guard.
func ValidateOAuthReauthorizationUpdate(ctx context.Context, current *Account, expected time.Time, credentials map[string]any) error {
	if current == nil || expected.IsZero() {
		return ErrOAuthReauthorizationStale
	}
	proof, bound := ctx.Value(openAIReauthorizationProofKey{}).(OpenAIOAuthSession)
	if bound && proof.ReauthorizationAccountRevision != "" {
		if proof.ReauthorizationRevision != expected.UTC().Format(time.RFC3339Nano) {
			return ErrOpenAIOAuthReauthorizationProofRequired
		}
	} else if !current.UpdatedAt.Equal(expected) {
		return ErrOAuthReauthorizationStale
	}
	return ValidateOpenAIOAuthReauthorizationCommit(ctx, current, credentials)
}

// The context value is never decoded from client JSON. Keep the same identity
// comparison at commit as at browser launch, exchange and proof consumption.
func ValidateOpenAIOAuthReauthorizationCommit(ctx context.Context, current *Account, credentials map[string]any) error {
	proof, ok := ctx.Value(openAIReauthorizationProofKey{}).(OpenAIOAuthSession)
	if !IsOpenAIBrowserOAuthAccount(current) {
		if ok {
			return ErrOAuthReauthorizationStale
		}
		return nil
	}
	// Manual token imports retain the existing timestamp and identity checks.
	if !ok {
		return nil
	}
	if proof.ReauthorizationAccountID != current.ID ||
		validateOpenAIOAuthReauthorizationBinding(&proof) != nil ||
		proof.ProxyID != proxyIDValue(current.ProxyID) ||
		proof.CreatedAt.IsZero() || proof.CreatedAt.After(time.Now()) ||
		!time.Now().Before(proof.CreatedAt.Add(openAIReauthorizationProofTTL)) {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	if proof.ReauthorizationAccountRevision != "" {
		if OpenAIOAuthAccountRevision(current) != proof.ReauthorizationAccountRevision {
			return ErrOAuthReauthorizationStale
		}
	} else if proof.ReauthorizationRevision != current.UpdatedAt.UTC().Format(time.RFC3339Nano) {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	hash, err := openAIReauthorizationCredentialsHash(credentials)
	if err != nil || hash != proof.ReauthorizationCredentialsHash {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	return nil
}
