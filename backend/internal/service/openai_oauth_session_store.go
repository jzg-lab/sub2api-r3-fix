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
	ID                              string
	State                           string
	CodeVerifier                    string
	ClientID                        string
	RedirectURI                     string
	ProxyID                         int64
	ProxyRouteHash                  string
	Platform                        string
	CreatedAt                       time.Time
	ReauthorizationAccountID        int64
	ReauthorizationRevision         string
	ReauthorizationAccountRevision  string
	ReauthorizationExitIP           string
	ReauthorizationCredentialsHash  string
	ReauthorizationBrowserSessionID string
	LoginExitIP                     string
	LoginBrowserSessionID           string
	LoginCredentialsHash            string
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
	if err := validateOpenAIOAuthReauthorizationBinding(session); err != nil {
		return err
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
				"state":                              session.State,
				"code_verifier":                      session.CodeVerifier,
				"client_id":                          session.ClientID,
				"redirect_uri":                       session.RedirectURI,
				"proxy_id":                           strconv.FormatInt(session.ProxyID, 10),
				"proxy_route_hash":                   session.ProxyRouteHash,
				"platform":                           session.Platform,
				"created_at":                         session.CreatedAt.UTC().Format(time.RFC3339Nano),
				"reauthorization_account_id":         strconv.FormatInt(session.ReauthorizationAccountID, 10),
				"reauthorization_revision":           session.ReauthorizationRevision,
				"reauthorization_account_revision":   session.ReauthorizationAccountRevision,
				"reauthorization_exit_ip":            session.ReauthorizationExitIP,
				"reauthorization_credentials_hash":   session.ReauthorizationCredentialsHash,
				"reauthorization_browser_session_id": session.ReauthorizationBrowserSessionID,
				"login_exit_ip":                      session.LoginExitIP,
				"login_browser_session_id":           session.LoginBrowserSessionID,
				"login_credentials_hash":             session.LoginCredentialsHash,
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

	result := &OpenAIOAuthSession{
		ID:             session.SessionToken,
		State:          oauthSessionStringValue(raw["state"]),
		CodeVerifier:   oauthSessionStringValue(raw["code_verifier"]),
		ClientID:       oauthSessionStringValue(raw["client_id"]),
		RedirectURI:    oauthSessionStringValue(raw["redirect_uri"]),
		ProxyID:        proxyID,
		ProxyRouteHash: oauthSessionStringValue(raw["proxy_route_hash"]),
		Platform:       oauthSessionStringValue(raw["platform"]),
		CreatedAt:      createdAt,
	}
	for key, target := range map[string]*string{
		"login_exit_ip":                      &result.LoginExitIP,
		"login_browser_session_id":           &result.LoginBrowserSessionID,
		"login_credentials_hash":             &result.LoginCredentialsHash,
		"reauthorization_browser_session_id": &result.ReauthorizationBrowserSessionID,
		"reauthorization_account_revision":   &result.ReauthorizationAccountRevision,
	} {
		if rawValue, exists := raw[key]; exists {
			value, valid := rawValue.(string)
			if !valid {
				return nil, ErrOpenAIOAuthSessionInvalid
			}
			*target = value
		}
	}
	rawID, hasID := raw["reauthorization_account_id"]
	rawRevision, hasRevision := raw["reauthorization_revision"]
	rawIP, hasIP := raw["reauthorization_exit_ip"]
	if rawHash, exists := raw["reauthorization_credentials_hash"]; exists {
		hash, ok := rawHash.(string)
		if !ok {
			return nil, ErrOpenAIOAuthSessionInvalid
		}
		result.ReauthorizationCredentialsHash = hash
	}
	if hasID || hasRevision || hasIP {
		idText, idOK := rawID.(string)
		revision, revisionOK := rawRevision.(string)
		ip, ipOK := rawIP.(string)
		id, parseErr := strconv.ParseInt(idText, 10, 64)
		if !hasID || !hasRevision || !hasIP || !idOK || !revisionOK || !ipOK || parseErr != nil {
			return nil, ErrOpenAIOAuthSessionInvalid
		}
		result.ReauthorizationAccountID = id
		result.ReauthorizationRevision = revision
		result.ReauthorizationExitIP = ip
	}
	if err := validateOpenAIOAuthReauthorizationBinding(result); err != nil {
		return nil, err
	}
	return result, nil
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
