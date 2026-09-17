package service

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

// OpenAIOAuthService handles OpenAI OAuth authentication flows
type OpenAIOAuthService struct {
	sessionStore         OpenAIOAuthSessionStore
	proxyRepo            ProxyRepository
	oauthClient          OpenAIOAuthClient
	tlsProfiles          *TLSFingerprintProfileService
	privacyClientFactory PrivacyClientFactory // 用于调用 chatgpt.com/backend-api（ImpersonateChrome）
}

// NewOpenAIOAuthService creates a new OpenAI OAuth service
func NewOpenAIOAuthService(proxyRepo ProxyRepository, oauthClient OpenAIOAuthClient) *OpenAIOAuthService {
	return &OpenAIOAuthService{
		proxyRepo:   proxyRepo,
		oauthClient: oauthClient,
	}
}

// SetSessionStore injects the durable OAuth session store (P0-14). Sessions
// persist in pending_auth_sessions with a 2h TTL so slow 接码 flows and
// process restarts no longer lose in-flight authorizations.
func (s *OpenAIOAuthService) SetSessionStore(store OpenAIOAuthSessionStore) {
	s.sessionStore = store
}

// SetPrivacyClientFactory 注入 ImpersonateChrome 客户端工厂，
// 用于调用 chatgpt.com/backend-api 获取账号信息（plan_type 等）。
func (s *OpenAIOAuthService) SetPrivacyClientFactory(factory PrivacyClientFactory) {
	s.privacyClientFactory = factory
}

func (s *OpenAIOAuthService) profileContext(ctx context.Context, account *Account) context.Context {
	if account != nil {
		var profile *tlsfingerprint.Profile
		if s.tlsProfiles != nil {
			profile = s.tlsProfiles.ResolveTLSProfile(account)
		}
		return tlsfingerprint.WithProfile(ctx, profile)
	}
	if _, present := tlsfingerprint.ProfileFromContext(ctx); present {
		return ctx
	}
	return tlsfingerprint.WithProfile(ctx, s.tlsProfiles.OpenAIOAuthDefaultProfile())
}

// OpenAIAuthURLResult contains the authorization URL and session info
type OpenAIAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
	ProxyID   int64  `json:"proxy_id"`
}

// GenerateAuthURL generates an OpenAI OAuth authorization URL
func (s *OpenAIOAuthService) GenerateAuthURL(ctx context.Context, proxyID *int64, redirectURI, platform string) (*OpenAIAuthURLResult, error) {
	if s.sessionStore == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_STORE_UNAVAILABLE", "openai oauth persistent session store is not configured")
	}
	// P0-14: 强制代理——授权浏览器出口、服务端 code→token 交换出口、账号常驻
	// 出口三者必须一致，否则 OpenAI 可凭 IP 不一致拒绝或标记账号。
	if proxyID == nil || *proxyID <= 0 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_REQUIRED", "a proxy is required for OpenAI OAuth")
	}

	// Generate PKCE values
	state, err := openai.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_STATE_FAILED", "failed to generate state: %v", err)
	}

	codeVerifier, err := openai.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_VERIFIER_FAILED", "failed to generate code verifier: %v", err)
	}

	codeChallenge := openai.GenerateCodeChallenge(codeVerifier)

	// Generate session ID
	sessionID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_FAILED", "failed to generate session ID: %v", err)
	}

	proxyURL, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, proxyID)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_INVALID", err.Error())
	}

	// Use default redirect URI if not specified
	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	normalizedPlatform := normalizeOpenAIOAuthPlatform(platform)
	clientID, _ := openai.OAuthClientConfigByPlatform(normalizedPlatform)

	// Store session durably
	session := &OpenAIOAuthSession{
		State:          state,
		CodeVerifier:   codeVerifier,
		ClientID:       clientID,
		RedirectURI:    redirectURI,
		ProxyID:        *proxyID,
		ProxyRouteHash: openAIOAuthProxyRouteHash(proxyURL),
		Platform:       normalizedPlatform,
		CreatedAt:      time.Now(),
	}
	session.ID = sessionID
	if err := s.sessionStore.Create(ctx, session); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_PERSIST_FAILED", "failed to persist oauth session: %v", err)
	}

	// Build authorization URL
	authURL := openai.BuildAuthorizationURLForPlatform(state, codeChallenge, redirectURI, normalizedPlatform)

	return &OpenAIAuthURLResult{
		AuthURL:   authURL,
		SessionID: sessionID,
		ProxyID:   session.ProxyID,
	}, nil
}

// OpenAIExchangeCodeInput represents the input for code exchange
type OpenAIExchangeCodeInput struct {
	SessionID   string
	Code        string
	State       string
	RedirectURI string
	ProxyID     *int64
}

// OpenAITokenInfo represents the token information for OpenAI
type OpenAITokenInfo struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	IDToken               string `json:"id_token,omitempty"`
	ExpiresIn             int64  `json:"expires_in"`
	ExpiresAt             int64  `json:"expires_at"`
	ClientID              string `json:"client_id,omitempty"`
	ProxyID               int64  `json:"proxy_id,omitempty"`
	AuthMode              string `json:"auth_mode,omitempty"`
	Email                 string `json:"email,omitempty"`
	ChatGPTAccountID      string `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID         string `json:"chatgpt_user_id,omitempty"`
	ChatGPTAccountFedRAMP bool   `json:"chatgpt_account_is_fedramp,omitempty"`
	OrganizationID        string `json:"organization_id,omitempty"`
	PlanType              string `json:"plan_type,omitempty"`
	SubscriptionExpiresAt string `json:"subscription_expires_at,omitempty"`
	PrivacyMode           string `json:"privacy_mode,omitempty"`
}

// ExchangeCode exchanges authorization code for tokens
func (s *OpenAIOAuthService) ExchangeCode(ctx context.Context, input *OpenAIExchangeCodeInput) (*OpenAITokenInfo, error) {
	ctx = s.profileContext(ctx, nil)
	if s.sessionStore == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_STORE_UNAVAILABLE", "openai oauth persistent session store is not configured")
	}
	if input == nil || strings.TrimSpace(input.SessionID) == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_REQUIRED", "session_id is required")
	}
	session, err := s.sessionStore.Get(ctx, input.SessionID)
	if err != nil || session == nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_NOT_FOUND", "session not found or expired")
	}
	if input.State == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_STATE_REQUIRED", "oauth state is required")
	}
	if subtle.ConstantTimeCompare([]byte(input.State), []byte(session.State)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_STATE", "invalid oauth state")
	}

	// P0-14: 交换必须与授权会话同一个代理出口，禁止换 IP 交换。
	if input.ProxyID != nil && *input.ProxyID != session.ProxyID {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_MISMATCH", "oauth proxy does not match the authorization session")
	}

	// Use redirect URI from session; a conflicting input is rejected outright.
	redirectURI := session.RedirectURI
	if strings.TrimSpace(input.RedirectURI) != "" && input.RedirectURI != redirectURI {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_REDIRECT_MISMATCH", "oauth redirect URI does not match the authorization session")
	}
	proxyURL, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, &session.ProxyID)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_INVALID", err.Error())
	}
	if session.ProxyRouteHash == "" || session.ProxyRouteHash != openAIOAuthProxyRouteHash(proxyURL) {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_CHANGED", "oauth proxy configuration changed; start a new authorization")
	}
	clientID := strings.TrimSpace(session.ClientID)
	if clientID == "" {
		clientID = openai.ClientID
	}

	// Consume before exchanging so the code verifier is one-shot even when two
	// browser callbacks race. A failed exchange requires a fresh authorization.
	expectedSession := *session
	session, err = s.sessionStore.Consume(ctx, input.SessionID)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_CONSUMED", "session not found, expired, or already used")
	}
	if session == nil || *session != expectedSession {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_CHANGED", "oauth session changed; start a new authorization")
	}
	proxyURL, err = resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, &session.ProxyID)
	if err != nil || session.ProxyRouteHash != openAIOAuthProxyRouteHash(proxyURL) {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_CHANGED", "oauth proxy configuration changed; start a new authorization")
	}

	// Exchange code for token
	tokenResp, err := s.oauthClient.ExchangeCode(ctx, input.Code, session.CodeVerifier, redirectURI, proxyURL, clientID)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// An administrator can change or remove the route while exchange is in flight.
	currentProxyURL, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, &session.ProxyID)
	if err != nil || session.ProxyRouteHash != openAIOAuthProxyRouteHash(currentProxyURL) {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_CHANGED", "oauth proxy configuration changed; start a new authorization")
	}
	if tokenResp == nil {
		return nil, infraerrors.New(http.StatusBadGateway, "OPENAI_OAUTH_EMPTY_RESPONSE", "oauth provider returned an empty response")
	}

	// Parse ID token to get user info
	var userInfo *openai.UserInfo
	if tokenResp.IDToken != "" {
		claims, parseErr := openai.ParseIDToken(tokenResp.IDToken)
		if parseErr != nil {
			slog.Warn("openai_oauth_id_token_parse_failed", "error", parseErr)
		} else {
			userInfo = claims.GetUserInfo()
		}
	}

	// 会话已在交换前一次性消费（Consume），无需再删除。

	tokenInfo := &OpenAITokenInfo{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    int64(tokenResp.ExpiresIn),
		ExpiresAt:    time.Now().Unix() + int64(tokenResp.ExpiresIn),
		ClientID:     clientID,
		ProxyID:      session.ProxyID,
	}

	if userInfo != nil {
		tokenInfo.Email = userInfo.Email
		tokenInfo.ChatGPTAccountID = userInfo.ChatGPTAccountID
		tokenInfo.ChatGPTUserID = userInfo.ChatGPTUserID
		tokenInfo.OrganizationID = userInfo.OrganizationID
		tokenInfo.PlanType = userInfo.PlanType
	}

	s.enrichTokenInfo(ctx, tokenInfo, proxyURL)

	return tokenInfo, nil
}

// RefreshToken refreshes an OpenAI OAuth token
func (s *OpenAIOAuthService) RefreshToken(ctx context.Context, refreshToken string, proxyURL string) (*OpenAITokenInfo, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

// RefreshTokenWithProxyID keeps the admin refresh endpoint on the same
// assignment validation path as authorization exchange and account refresh.
func (s *OpenAIOAuthService) RefreshTokenWithProxyID(ctx context.Context, refreshToken string, proxyID *int64, clientID string) (*OpenAITokenInfo, error) {
	proxyURL, err := resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, proxyID)
	if err != nil {
		return nil, err
	}
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

// RefreshTokenWithClientID refreshes an OpenAI OAuth token with optional client_id.
func (s *OpenAIOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken string, proxyURL string, clientID string) (*OpenAITokenInfo, error) {
	if err := validateOpenAIOAuthProxyURL(proxyURL); err != nil {
		return nil, err
	}
	ctx = s.profileContext(ctx, nil)
	tokenResp, err := s.oauthClient.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
	if err != nil {
		return nil, err
	}

	// Parse ID token to get user info
	var userInfo *openai.UserInfo
	if tokenResp.IDToken != "" {
		claims, parseErr := openai.ParseIDToken(tokenResp.IDToken)
		if parseErr != nil {
			slog.Warn("openai_oauth_id_token_parse_failed", "error", parseErr)
		} else {
			userInfo = claims.GetUserInfo()
		}
	}

	tokenInfo := &OpenAITokenInfo{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    int64(tokenResp.ExpiresIn),
		ExpiresAt:    time.Now().Unix() + int64(tokenResp.ExpiresIn),
	}
	if trimmed := strings.TrimSpace(clientID); trimmed != "" {
		tokenInfo.ClientID = trimmed
	}

	if userInfo != nil {
		tokenInfo.Email = userInfo.Email
		tokenInfo.ChatGPTAccountID = userInfo.ChatGPTAccountID
		tokenInfo.ChatGPTUserID = userInfo.ChatGPTUserID
		tokenInfo.OrganizationID = userInfo.OrganizationID
		tokenInfo.PlanType = userInfo.PlanType
	}

	s.enrichTokenInfo(ctx, tokenInfo, proxyURL)

	return tokenInfo, nil
}

// enrichTokenInfo 通过 ChatGPT backend-api 补全 tokenInfo 并设置隐私（best-effort）。
// 从 accounts/check 获取最新 plan_type、subscription_expires_at、email，
// 然后尝试关闭训练数据共享。适用于所有获取/刷新 token 的路径。
func (s *OpenAIOAuthService) enrichTokenInfo(ctx context.Context, tokenInfo *OpenAITokenInfo, proxyURL string) {
	if tokenInfo.AccessToken == "" || s.privacyClientFactory == nil {
		return
	}

	// 从 access_token JWT 中提取 orgID（poid），用于匹配正确的账号
	orgID := tokenInfo.OrganizationID
	if orgID == "" {
		if atClaims, err := openai.DecodeIDToken(tokenInfo.AccessToken); err == nil && atClaims.OpenAIAuth != nil {
			orgID = atClaims.OpenAIAuth.POID
		}
	}
	// accounts/check 命中的记录不属于个人账号时，必须改用个人订阅端点拿到期时间，
	// 否则会把 workspace 权益的 expires_at 当成个人订阅到期日展示。
	forcePersonalSubscriptionLookup := false
	if info := fetchChatGPTAccountInfo(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, orgID); info != nil {
		// chatgpt_plan_type from the ID token is the canonical personal-plan value.
		// accounts/check is a multi-account/workspace endpoint; inactive team or
		// business workspaces can otherwise overwrite Pro/Free with internal
		// workspace billing plan names such as self_serve_business_usage_based.
		appliedAccountInfoPlanType := shouldApplyChatGPTAccountInfoPlanType(tokenInfo.PlanType, info.PlanType)
		if appliedAccountInfoPlanType {
			tokenInfo.PlanType = info.PlanType
		}
		// plan_type 与 subscription_expires_at 必须描述同一份订阅。套餐取自
		// accounts/check 时，到期时间跟着取同一条记录；套餐保留了 JWT 里的个人值时，
		// 只有该记录确实就是个人账号才能用它的 entitlement.expires_at——poid 指向的
		// 默认 Personal workspace 与 chatgpt_account_id 可以是两个不同的标识，
		// 混用会显示成「个人 Pro + workspace 到期时间」。
		if info.SubscriptionExpiresAt != "" {
			if appliedAccountInfoPlanType || chatGPTAccountInfoBelongsToTokenAccount(tokenInfo, info) {
				tokenInfo.SubscriptionExpiresAt = info.SubscriptionExpiresAt
			} else {
				forcePersonalSubscriptionLookup = true
			}
		}
		if tokenInfo.Email == "" && info.Email != "" {
			tokenInfo.Email = info.Email
		}
	}
	if forcePersonalSubscriptionLookup || strings.TrimSpace(tokenInfo.SubscriptionExpiresAt) == "" {
		if expiresAt := fetchChatGPTSubscriptionExpiresAt(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, resolveChatGPTSubscriptionAccountID(tokenInfo, orgID)); expiresAt != "" {
			tokenInfo.SubscriptionExpiresAt = expiresAt
		}
	}

	// 尝试设置隐私（关闭训练数据共享），best-effort
	tokenInfo.PrivacyMode = disableOpenAITraining(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL)
}

func shouldApplyChatGPTAccountInfoPlanType(current, candidate string) bool {
	return strings.TrimSpace(candidate) != "" && strings.TrimSpace(current) == ""
}

// chatGPTAccountInfoBelongsToTokenAccount 判断 accounts/check 命中的那条记录是不是
// token 自己的个人 ChatGPT 账号。两侧任一缺 ID 时无法区分，返回 true 保持既有行为。
func chatGPTAccountInfoBelongsToTokenAccount(tokenInfo *OpenAITokenInfo, info *ChatGPTAccountInfo) bool {
	personalID := strings.TrimSpace(tokenInfo.ChatGPTAccountID)
	sourceID := strings.TrimSpace(info.AccountID)
	if personalID == "" || sourceID == "" {
		return true
	}
	return strings.EqualFold(personalID, sourceID)
}

func resolveChatGPTSubscriptionAccountID(tokenInfo *OpenAITokenInfo, orgID string) string {
	for _, candidate := range []string{
		tokenInfo.ChatGPTAccountID,
		tokenInfo.OrganizationID,
		orgID,
	} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// RefreshAccountToken refreshes token for an OpenAI OAuth account
func (s *OpenAIOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	ctx = s.profileContext(ctx, account)
	if account.Platform != PlatformOpenAI {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT", "account is not an OpenAI account")
	}
	if account.Type != AccountTypeOAuth {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT_TYPE", "account is not an OAuth account")
	}

	var proxyURL string
	// PAT imports are not browser OAuth grants. Preserve their explicit direct
	// mode, but never bypass a proxy that was assigned to either account type.
	if !account.IsOpenAIPersonalAccessToken() || account.ProxyID != nil {
		var err error
		proxyURL, err = resolveOpenAIOAuthProxyURL(ctx, s.proxyRepo, account.ProxyID)
		if err != nil {
			return nil, err
		}
	}

	accessToken := account.GetCredential("access_token")
	if account.IsOpenAIPersonalAccessToken() {
		if accessToken == "" {
			return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_PAT_REQUIRED", "access token is required")
		}
		return s.ValidateCodexPersonalAccessToken(ctx, accessToken, proxyURL)
	}

	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		if accessToken != "" {
			tokenInfo := &OpenAITokenInfo{
				AccessToken:           accessToken,
				RefreshToken:          "",
				IDToken:               account.GetCredential("id_token"),
				ClientID:              account.GetCredential("client_id"),
				Email:                 account.GetCredential("email"),
				ChatGPTAccountID:      account.GetCredential("chatgpt_account_id"),
				ChatGPTUserID:         account.GetCredential("chatgpt_user_id"),
				OrganizationID:        account.GetCredential("organization_id"),
				PlanType:              account.GetCredential("plan_type"),
				SubscriptionExpiresAt: account.GetCredential("subscription_expires_at"),
			}
			if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
				tokenInfo.ExpiresAt = expiresAt.Unix()
				tokenInfo.ExpiresIn = int64(time.Until(*expiresAt).Seconds())
			}
			s.enrichTokenInfo(ctx, tokenInfo, proxyURL)
			return tokenInfo, nil
		}
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_NO_REFRESH_TOKEN", "no refresh token available")
	}

	clientID := account.GetCredential("client_id")
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

// BuildAccountCredentials builds credentials map from token info
func (s *OpenAIOAuthService) BuildAccountCredentials(tokenInfo *OpenAITokenInfo) map[string]any {
	creds := map[string]any{
		"access_token": tokenInfo.AccessToken,
	}
	if tokenInfo.ExpiresAt > 0 {
		creds["expires_at"] = time.Unix(tokenInfo.ExpiresAt, 0).Format(time.RFC3339)
	}
	// 仅在刷新响应返回了新的 refresh_token 时才更新，防止用空值覆盖已有令牌
	if strings.TrimSpace(tokenInfo.RefreshToken) != "" {
		creds["refresh_token"] = tokenInfo.RefreshToken
	}

	if tokenInfo.IDToken != "" {
		creds["id_token"] = tokenInfo.IDToken
	}
	if tokenInfo.Email != "" {
		creds["email"] = tokenInfo.Email
	}
	if tokenInfo.ChatGPTAccountID != "" {
		creds["chatgpt_account_id"] = tokenInfo.ChatGPTAccountID
	}
	if tokenInfo.ChatGPTUserID != "" {
		creds["chatgpt_user_id"] = tokenInfo.ChatGPTUserID
	}
	if tokenInfo.OrganizationID != "" {
		creds["organization_id"] = tokenInfo.OrganizationID
	}
	if tokenInfo.PlanType != "" {
		creds["plan_type"] = tokenInfo.PlanType
	}
	if tokenInfo.SubscriptionExpiresAt != "" {
		creds["subscription_expires_at"] = tokenInfo.SubscriptionExpiresAt
	}
	if strings.TrimSpace(tokenInfo.ClientID) != "" {
		creds["client_id"] = strings.TrimSpace(tokenInfo.ClientID)
	}
	if tokenInfo.AuthMode == OpenAIAuthModePersonalAccessToken {
		creds[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
		creds[openAIAuthModeLegacyCredentialKey] = "personal_access_token"
		creds["token_type"] = "Bearer"
		creds["chatgpt_account_is_fedramp"] = tokenInfo.ChatGPTAccountFedRAMP
	} else if tokenInfo.ChatGPTAccountFedRAMP {
		creds["chatgpt_account_is_fedramp"] = true
	}

	return NormalizeOpenAIPersonalAccessTokenCredentials(nil, tokenInfo, creds)
}

// Stop is retained for lifecycle compatibility. Durable pending auth sessions
// have no goroutine of their own; they are cleaned up by the database cleanup path.
func (s *OpenAIOAuthService) Stop() {
}

func normalizeOpenAIOAuthPlatform(platform string) string {
	return openai.OAuthPlatformOpenAI
}
