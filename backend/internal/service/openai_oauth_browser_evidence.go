package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

func authorizationBrowserProofError(session *OpenAIOAuthSession) error {
	if session.ReauthorizationAccountID != 0 {
		return ErrOpenAIOAuthReauthorizationProofRequired
	}
	return ErrOpenAIOAuthInitialLoginProofRequired
}

func authorizationBrowserEvidence(session *OpenAIOAuthSession) OpenAIOAuthSession {
	if session.ReauthorizationAccountID == 0 {
		return initialLoginBrowserEvidence(session)
	}
	evidence := *session
	sum := sha256.Sum256([]byte("openai-reauthorization-browser:" + session.ID))
	evidence.ID = hex.EncodeToString(sum[:])
	evidence.State = ""
	evidence.CodeVerifier = ""
	evidence.ReauthorizationBrowserSessionID = session.ID
	return evidence
}

// Both flows require durable evidence from this exact session's fixed-route
// launcher. An exit probe at exchange time cannot attest to a browser launch.
func (s *OpenAIOAuthService) recordAuthorizationBrowser(ctx context.Context, session *OpenAIOAuthSession) error {
	if err := s.validateReauthorizationSession(ctx, session, false); err != nil {
		return err
	}
	evidence := authorizationBrowserEvidence(session)
	existing, err := s.sessionStore.Get(ctx, evidence.ID)
	if err == nil {
		if existing != nil && *existing == evidence {
			return nil
		}
		return authorizationBrowserProofError(session)
	}
	if !errors.Is(err, ErrPendingAuthSessionNotFound) {
		return err
	}
	return s.sessionStore.Create(ctx, &evidence)
}

func (s *OpenAIOAuthService) consumeAuthorizationBrowser(ctx context.Context, session *OpenAIOAuthSession) error {
	if session.ReauthorizationAccountID == 0 && session.LoginExitIP == "" {
		return nil
	}
	expected := authorizationBrowserEvidence(session)
	evidence, err := s.sessionStore.Consume(ctx, expected.ID)
	if err != nil || evidence == nil || *evidence != expected {
		return authorizationBrowserProofError(session)
	}
	return nil
}
