package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

var ErrOpenAIOAuthInitialLoginProofRequired = infraerrors.Conflict(
	"OPENAI_OAUTH_INITIAL_LOGIN_PROOF_REQUIRED",
	"complete authorization in the verified fixed-egress browser before recording the login IP",
)

type openAIInitialLoginProofKey struct{}

func validateOpenAIInitialLoginBinding(session *OpenAIOAuthSession) error {
	if session.LoginExitIP == "" {
		if session.LoginBrowserSessionID != "" || session.LoginCredentialsHash != "" {
			return ErrOpenAIOAuthSessionInvalid
		}
		return nil
	}
	ip, err := normalizeOpenAIOAuthLoginIP(session.LoginExitIP)
	if err != nil || ip != session.LoginExitIP || session.ReauthorizationAccountID != 0 ||
		session.Platform != PlatformOpenAI || !validOpenAIAuthBrowserLowerHex(session.ProxyRouteHash, 64) {
		return ErrOpenAIOAuthSessionInvalid
	}
	if session.LoginBrowserSessionID != "" {
		if !validOpenAIAuthBrowserLowerHex(session.LoginBrowserSessionID, 32) ||
			session.State != "" || session.CodeVerifier != "" {
			return ErrOpenAIOAuthSessionInvalid
		}
	}
	if session.LoginCredentialsHash != "" &&
		(session.LoginBrowserSessionID == "" || !validOpenAIAuthBrowserLowerHex(session.LoginCredentialsHash, 64)) {
		return ErrOpenAIOAuthSessionInvalid
	}
	return nil
}

func (s *OpenAIOAuthService) validateInitialLoginSession(ctx context.Context, session *OpenAIOAuthSession, probe bool) error {
	if session.LoginExitIP == "" {
		return nil
	}
	route, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, &session.ProxyID)
	pin, ok := s.fixedEgressRoutes[session.ProxyID]
	if err != nil || !ok || pin.ExitIP != session.LoginExitIP ||
		pin.ProxyRouteSHA256 != session.ProxyRouteHash || openAIOAuthProxyRouteHash(route) != session.ProxyRouteHash {
		return ErrOpenAIOAuthFixedEgressRequired
	}
	if !probe {
		return nil
	}
	observe := s.observeReauthorizationExitIP
	if observe == nil {
		observe = observeOpenAIOAuthExitIP
	}
	actual, err := observe(ctx, route)
	if err != nil {
		return ErrOpenAIOAuthLoginIPUnavailable
	}
	ip, err := normalizeOpenAIOAuthLoginIP(actual)
	if err != nil || ip != session.LoginExitIP {
		return ErrOpenAIOAuthLoginIPChanged
	}
	return nil
}

// Separate durable evidence prevents a copied URL or a caller-supplied IP from
// establishing history. Only the host launcher writes this after a successful
// fixed-egress launch; the exchange consumes it together with the OAuth session.
func initialLoginBrowserEvidence(session *OpenAIOAuthSession) OpenAIOAuthSession {
	evidence := *session
	sum := sha256.Sum256([]byte("openai-initial-login-browser:" + session.ID))
	evidence.ID = hex.EncodeToString(sum[:])
	evidence.State = ""
	evidence.CodeVerifier = ""
	evidence.LoginBrowserSessionID = session.ID
	return evidence
}

func (s *OpenAIOAuthService) issueInitialAuthorizationProof(ctx context.Context, session *OpenAIOAuthSession, result *OpenAITokenInfo) error {
	if session.LoginExitIP == "" {
		return nil
	}
	hash, err := openAIReauthorizationCredentialsHash(s.BuildAccountCredentials(result))
	if err != nil {
		return ErrOpenAIOAuthInitialLoginProofRequired
	}
	id, err := openai.GenerateSessionID()
	if err != nil {
		return err
	}
	proof := initialLoginBrowserEvidence(session)
	proof.ID = id
	proof.LoginCredentialsHash = hash
	proof.CreatedAt = time.Now().UTC()
	if err := s.sessionStore.Create(ctx, &proof); err != nil {
		return err
	}
	result.InitialAuthorizationProof = id
	return nil
}

func initialLoginProofMatches(proof *OpenAIOAuthSession, account *Account) bool {
	if proof == nil || !IsOpenAIBrowserOAuthAccount(account) || account.ProxyID == nil ||
		*account.ProxyID != proof.ProxyID || proof.LoginExitIP == "" || proof.LoginCredentialsHash == "" ||
		validateOpenAIOAuthReauthorizationBinding(proof) != nil {
		return false
	}
	now := time.Now()
	hash, err := openAIReauthorizationCredentialsHash(account.Credentials)
	return err == nil && hash == proof.LoginCredentialsHash && !proof.CreatedAt.After(now) &&
		now.Before(proof.CreatedAt.Add(openAIReauthorizationProofTTL))
}

func (s *OpenAIOAuthService) ConsumeInitialAuthorizationProof(ctx context.Context, id string, account *Account) (context.Context, error) {
	if id == "" {
		return ctx, nil
	}
	if s == nil || s.sessionStore == nil {
		return nil, ErrOpenAIOAuthInitialLoginProofRequired
	}
	proof, err := s.sessionStore.Get(ctx, id)
	if err != nil || proof == nil || proof.ID != id || !initialLoginProofMatches(proof, account) {
		return nil, ErrOpenAIOAuthInitialLoginProofRequired
	}
	if err := s.validateInitialLoginSession(ctx, proof, true); err != nil {
		return nil, err
	}
	expected := *proof
	consumed, err := s.sessionStore.Consume(ctx, id)
	if err != nil || consumed == nil || *consumed != expected {
		return nil, ErrOpenAIOAuthInitialLoginProofRequired
	}
	return context.WithValue(ctx, openAIInitialLoginProofKey{}, expected), nil
}

// Called inside the create transaction. No client JSON field can grant this
// capability, and an initial login must never backfill a missing historical IP.
func OpenAIOAuthInitialLoginIPForCreate(ctx context.Context, account *Account, historyFound bool, historicalIP string) (string, error) {
	proof, ok := ctx.Value(openAIInitialLoginProofKey{}).(OpenAIOAuthSession)
	if !ok {
		return "", nil
	}
	if !initialLoginProofMatches(&proof, account) {
		return "", ErrOpenAIOAuthInitialLoginProofRequired
	}
	if historyFound && historicalIP != proof.LoginExitIP {
		return "", ErrOpenAIOAuthLoginIPChanged
	}
	return proof.LoginExitIP, nil
}

func ValidateOpenAIOAuthInitialLoginProxy(ctx context.Context, proxy *Proxy) error {
	proof, ok := ctx.Value(openAIInitialLoginProofKey{}).(OpenAIOAuthSession)
	if !ok {
		return nil
	}
	route, err := openAIOAuthProxySnapshotURL(proxy, &proof.ProxyID)
	if err != nil || openAIOAuthProxyRouteHash(route) != proof.ProxyRouteHash {
		return ErrOpenAIOAuthFixedEgressRequired
	}
	return nil
}
