//go:build integration

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountTOTPPersistenceAndIdentity(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	cfg := &config.Config{}
	cfg.Totp.EncryptionKey = strings.Repeat("42", 32)
	cfg.Totp.EncryptionKeyConfigured = true
	enc, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	secrets := service.NewAccountTOTPService(client, enc, cfg)
	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	account := &service.Account{Name: "totp-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Credentials: map[string]any{"email": "fixture@example.com", "access_token": "original"}, Status: service.StatusActive, Schedulable: false}
	require.NoError(t, repo.Create(ctx, account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
	})
	current, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	revision := service.OpenAIOAuthAccountRevision(current)
	const fixture = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	secret := fixture
	input := service.AccountTOTPInput{ExpectedRevision: revision, MFASecret: &secret}
	noKey := service.NewAccountTOTPService(client, enc, &config.Config{})
	_, err = noKey.Save(ctx, account.ID, input)
	require.ErrorIs(t, err, service.ErrAccountTOTPKey)
	status, err := secrets.Save(ctx, account.ID, input)
	require.NoError(t, err)
	require.True(t, status.HasSecret)
	var ciphertext string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT totp_secret_encrypted FROM accounts WHERE id=$1", account.ID).Scan(&ciphertext))
	require.NotContains(t, ciphertext, fixture)
	require.NotContains(t, ciphertext, "fixture@example.com")
	// Exercise the real HTTP contract and shared authentication boundary.
	handler := admin.NewOpenAIOAuthHandler(nil, nil, nil, nil)
	handler.SetAccountTOTP(secrets)
	router := gin.New()
	router.GET("/accounts/:id/totp", handler.AccountTOTP)
	router.PUT("/accounts/:id/totp", handler.AccountTOTP)
	payload, err := json.Marshal(input)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/accounts/%d/totp", account.ID), bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"has_totp_secret":true`)
	require.NotContains(t, recorder.Body.String(), fixture)
	require.NotContains(t, recorder.Body.String(), ciphertext)
	for _, body := range []string{`{"totp_secret":null}`, `{"mfa_secret":123}`, `{"unknown":"value"}`} {
		request := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/accounts/%d/totp", account.ID), strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
	protected := gin.New()
	protected.Use(gin.HandlerFunc(middleware.NewAdminAuthMiddleware(nil, nil, nil, nil)))
	protected.PUT("/accounts/:id/totp", handler.AccountTOTP)
	recorder = httptest.NewRecorder()
	protected.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/accounts/%d/totp", account.ID), bytes.NewReader(payload)))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	// A new service/encryptor instance can read after restart with the fixed key.
	enc2, err := NewAESEncryptor(cfg)
	require.NoError(t, err)
	secrets = service.NewAccountTOTPService(client, enc2, cfg)
	session := &service.OpenAIOAuthSession{ReauthorizationAccountID: account.ID, ReauthorizationAccountRevision: revision}
	got, err := secrets.Load(ctx, session, "fixture@example.com")
	require.NoError(t, err)
	require.Equal(t, fixture, got)
	_, err = secrets.Load(ctx, session, "other@example.com")
	require.ErrorIs(t, err, service.ErrAccountTOTPUnavailable)
	// Run the real launcher pipeline with a synthetic child and persisted secret.
	store := service.NewPendingAuthOpenAIOAuthSessionStore(service.NewAuthPendingIdentityService(client))
	oauth := service.NewOpenAIOAuthService(nil, nil)
	oauth.SetSessionStore(store)
	oauth.SetReauthorizationAccountLookup(repo.GetByID)
	generated, err := oauth.GenerateReauthorizationAuthURL(ctx, account.ID, current.UpdatedAt.Format(time.RFC3339Nano), nil, "", service.PlatformOpenAI, revision)
	require.NoError(t, err)
	bound, err := store.Get(ctx, generated.SessionID)
	require.NoError(t, err)
	launcherPath := filepath.Join(t.TempDir(), "login.sh")
	script := "#!/bin/sh\ninput=$(cat)\ncase \"$input\" in *" + fixture + "*) printf '%s' '{\"code\":\"fixture-code\",\"state\":\"" + bound.State + "\"}' ;; *) exit 1 ;; esac\n"
	require.NoError(t, os.WriteFile(launcherPath, []byte(script), 0700))
	cfg.Gateway.AuthBrowserLauncher = launcherPath
	launcher := service.NewOpenAIAuthBrowserLauncher(cfg, store, NewProxyRepository(client, integrationDB))
	launcher.SetOAuthService(oauth)
	launcher.SetAccountTOTP(secrets)
	login := &service.OpenAIAuthBrowserLogin{Email: "fixture@example.com", Password: "test-only", UseStoredTOTP: true}
	launched, err := launcher.LaunchWithLogin(ctx, generated.SessionID, login)
	require.NoError(t, err)
	require.True(t, launched.Launched)
	require.Equal(t, "fixture-code", launched.Code)
	require.Equal(t, "direct", launched.ExitIngress)
	require.Empty(t, login.TOTPSecret, "stored secret must not be copied back to the caller")
	handler.SetAuthBrowserLauncher(launcher)
	router.POST("/launch", handler.LaunchAuthBrowser)
	login.Email = "other@example.com"
	launchBody, err := json.Marshal(map[string]any{"session_id": generated.SessionID, "login": login})
	require.NoError(t, err)
	request = httptest.NewRequest(http.MethodPost, "/launch", bytes.NewReader(launchBody))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.NotContains(t, recorder.Body.String(), fixture)

	// Omitted fields preserve, refresh keeps the secret, old revisions reject.
	status, err = secrets.Save(ctx, account.ID, service.AccountTOTPInput{ExpectedRevision: revision})
	require.NoError(t, err)
	require.True(t, status.HasSecret)
	credentials := map[string]any{"email": "fixture@example.com", "access_token": "refreshed"}
	require.NoError(t, repo.UpdateCredentials(ctx, account.ID, credentials))
	_, err = secrets.Save(ctx, account.ID, input)
	require.ErrorIs(t, err, service.ErrOAuthReauthorizationStale)
	current, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.False(t, current.Schedulable)
	require.NotContains(t, current.Credentials, "totp_secret")
	require.NotContains(t, current.Extra, "totp_secret")
	session.ReauthorizationAccountRevision = service.OpenAIOAuthAccountRevision(current)
	got, err = secrets.Load(ctx, session, "fixture@example.com")
	require.NoError(t, err)
	require.Equal(t, fixture, got)
	wrongCfg := *cfg
	wrongCfg.Totp.EncryptionKey = strings.Repeat("43", 32)
	wrongEnc, err := NewAESEncryptor(&wrongCfg)
	require.NoError(t, err)
	_, err = service.NewAccountTOTPService(client, wrongEnc, &wrongCfg).Load(ctx, session, "fixture@example.com")
	require.ErrorIs(t, err, service.ErrAccountTOTPUnavailable)
	// Identity substitution cannot reuse the old encrypted secret.
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET credentials=jsonb_set(credentials,'{email}','"other@example.com"') WHERE id=$1`, account.ID)
	require.NoError(t, err)
	current, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	session.ReauthorizationAccountRevision = service.OpenAIOAuthAccountRevision(current)
	_, err = secrets.Load(ctx, session, "other@example.com")
	require.ErrorIs(t, err, service.ErrAccountTOTPUnavailable)
	status, err = noKey.Save(ctx, account.ID, service.AccountTOTPInput{ExpectedRevision: session.ReauthorizationAccountRevision, Clear: true})
	require.NoError(t, err)
	require.False(t, status.HasSecret)
}
