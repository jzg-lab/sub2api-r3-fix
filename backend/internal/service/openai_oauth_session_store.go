package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

const openAIOAuthSessionTTL = 2 * time.Hour

var ErrOpenAIOAuthSessionInvalid = errors.New("openai oauth session payload is invalid")

type OpenAIOAuthSession struct {
	ID             string
	State          string
	CodeVerifier   string
	ClientID       string
	RedirectURI    string
	ProxyID        int64
	ProxyRouteHash string
	Platform       string
	CreatedAt      time.Time
}

type OpenAIOAuthSessionStore interface {
	Create(ctx context.Context, session *OpenAIOAuthSession) error
	Get(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error)
	Consume(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error)
}

type pendingAuthOpenAIOAuthSessionStore struct {
	pending *AuthPendingIdentityService
}

func NewPendingAuthOpenAIOAuthSessionStore(pending *AuthPendingIdentityService) OpenAIOAuthSessionStore {
	if pending == nil {
		return nil
	}
	return &pendingAuthOpenAIOAuthSessionStore{pending: pending}
}

func (s *pendingAuthOpenAIOAuthSessionStore) Create(ctx context.Context, session *OpenAIOAuthSession) error {
	if s == nil || s.pending == nil {
		return fmt.Errorf("openai oauth persistent session store is not configured")
	}
	if session == nil || strings.TrimSpace(session.ID) == "" {
		return fmt.Errorf("openai oauth session is required")
	}
	if session.ProxyID <= 0 {
		return fmt.Errorf("openai oauth session proxy is required")
	}

	_, err := s.pending.CreatePendingSession(ctx, CreatePendingAuthSessionInput{
		SessionToken: session.ID,
		Intent:       "login",
		Identity: PendingAuthIdentityKey{
			ProviderType:    "oidc",
			ProviderKey:     "openai",
			ProviderSubject: session.ID,
		},
		RedirectTo: session.RedirectURI,
		ExpiresAt:  session.CreatedAt.UTC().Add(openAIOAuthSessionTTL),
		LocalFlowState: map[string]any{
			"openai_oauth": map[string]any{
				"state":            session.State,
				"code_verifier":    session.CodeVerifier,
				"client_id":        session.ClientID,
				"redirect_uri":     session.RedirectURI,
				"proxy_id":         strconv.FormatInt(session.ProxyID, 10),
				"proxy_route_hash": session.ProxyRouteHash,
				"platform":         session.Platform,
				"created_at":       session.CreatedAt.UTC().Format(time.RFC3339Nano),
			},
		},
	})
	return err
}

func (s *pendingAuthOpenAIOAuthSessionStore) Get(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	if s == nil || s.pending == nil {
		return nil, fmt.Errorf("openai oauth persistent session store is not configured")
	}
	session, err := s.pending.GetBrowserSession(ctx, sessionID, "")
	if err != nil {
		return nil, err
	}
	return decodeOpenAIOAuthSession(session)
}

func (s *pendingAuthOpenAIOAuthSessionStore) Consume(ctx context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	if s == nil || s.pending == nil {
		return nil, fmt.Errorf("openai oauth persistent session store is not configured")
	}
	session, err := s.pending.ConsumeBrowserSession(ctx, sessionID, "")
	if err != nil {
		return nil, err
	}
	return decodeOpenAIOAuthSession(session)
}

func decodeOpenAIOAuthSession(session *dbent.PendingAuthSession) (*OpenAIOAuthSession, error) {
	if session == nil {
		return nil, fmt.Errorf("%w: session is required", ErrOpenAIOAuthSessionInvalid)
	}
	raw, ok := session.LocalFlowState["openai_oauth"].(map[string]any)
	if !ok {
		return nil, ErrOpenAIOAuthSessionInvalid
	}

	proxyID, err := strconv.ParseInt(strings.TrimSpace(oauthSessionStringValue(raw["proxy_id"])), 10, 64)
	if err != nil || proxyID <= 0 {
		return nil, fmt.Errorf("%w: proxy is invalid", ErrOpenAIOAuthSessionInvalid)
	}
	createdAt, err := time.Parse(time.RFC3339Nano, oauthSessionStringValue(raw["created_at"]))
	if err != nil {
		return nil, fmt.Errorf("%w: creation time is invalid", ErrOpenAIOAuthSessionInvalid)
	}

	return &OpenAIOAuthSession{
		ID:             session.SessionToken,
		State:          oauthSessionStringValue(raw["state"]),
		CodeVerifier:   oauthSessionStringValue(raw["code_verifier"]),
		ClientID:       oauthSessionStringValue(raw["client_id"]),
		RedirectURI:    oauthSessionStringValue(raw["redirect_uri"]),
		ProxyID:        proxyID,
		ProxyRouteHash: oauthSessionStringValue(raw["proxy_route_hash"]),
		Platform:       oauthSessionStringValue(raw["platform"]),
		CreatedAt:      createdAt,
	}, nil
}

func oauthSessionStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(value)
	}
}

var _ OpenAIOAuthSessionStore = (*pendingAuthOpenAIOAuthSessionStore)(nil)
