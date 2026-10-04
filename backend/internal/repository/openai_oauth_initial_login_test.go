package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type initialLoginProofContext struct {
	context.Context
	proof service.OpenAIOAuthSession
}

func (c initialLoginProofContext) Value(key any) any {
	kind := reflect.TypeOf(key)
	if kind != nil && kind.PkgPath() == "github.com/Wei-Shaw/sub2api/internal/service" &&
		kind.Name() == "openAIInitialLoginProofKey" {
		return c.proof
	}
	return c.Context.Value(key)
}

func initialLoginCommitContext(t *testing.T, account *service.Account, proxy *service.Proxy) initialLoginProofContext {
	t.Helper()
	hash := reauthCommitContext(t, time.Now(), account.Credentials).proof.ReauthorizationCredentialsHash
	routeHash := sha256.Sum256([]byte(proxy.URL()))
	return initialLoginProofContext{Context: t.Context(), proof: service.OpenAIOAuthSession{
		Platform: service.PlatformOpenAI, ProxyID: proxy.ID, ProxyRouteHash: hex.EncodeToString(routeHash[:]),
		CreatedAt: time.Now().UTC(), LoginExitIP: "198.51.100.25",
		LoginBrowserSessionID: strings.Repeat("a", 32), LoginCredentialsHash: hash,
	}}
}

func TestOpenAIInitialLoginCreateChecksTransactionRouteAndHistory(t *testing.T) {
	for _, mutation := range []string{"valid", "route changed", "missing history", "different history", "same history"} {
		t.Run(mutation, func(t *testing.T) {
			db, mock := openAIOAuthPrepareMock(t)
			account := openAIOAuthCreateFixture()
			account.Credentials["access_token"] = "fixture"
			proxy := &service.Proxy{ID: 7, Protocol: "http", Host: "127.0.0.1", Port: 8080, Status: service.StatusActive}
			ctx := initialLoginCommitContext(t, account, proxy)
			account.Extra[service.OpenAIOAuthLoginExitIPExtraKey] = "198.51.100.99"
			expectOpenAIOAuthIdentityLock(mock)
			history := sqlmock.NewRows([]string{"credentials", "extra", "proxy_id", "deleted_at"})
			switch mutation {
			case "missing history":
				history.AddRow([]byte(`{"email":"user@example.com"}`),
					[]byte(`{"openai_oauth_qualified_proxy_id":7}`), int64(7), time.Now().Add(-time.Hour))
			case "different history", "same history":
				ip := "198.51.100.99"
				if mutation == "same history" {
					ip = "198.51.100.25"
				}
				history.AddRow([]byte(`{"email":"user@example.com"}`),
					[]byte(`{"openai_oauth_qualified_proxy_id":7,"openai_oauth_login_exit_ip":"`+ip+`"}`),
					int64(7), time.Now().Add(-time.Hour))
			}
			expectOpenAIOAuthHistory(mock, history)
			if mutation != "missing history" && mutation != "different history" {
				expectValidOpenAIOAuthProxy(mock, 7)
				if mutation == "route changed" {
					proxy.Port++
				}
				mock.ExpectQuery(`(?s)SELECT protocol, host, port,.*FROM proxies`).
					WithArgs(int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"protocol", "host", "port", "username", "password"}).
						AddRow(proxy.Protocol, proxy.Host, proxy.Port, "", ""))
			}
			err := prepareOpenAIOAuthAccountCreate(ctx, db, account)
			switch mutation {
			case "valid", "same history":
				require.NoError(t, err)
				require.Equal(t, "198.51.100.25", account.Extra[service.OpenAIOAuthLoginExitIPExtraKey])
			case "route changed":
				require.ErrorIs(t, err, service.ErrOpenAIOAuthFixedEgressRequired)
			default:
				require.ErrorIs(t, err, service.ErrOpenAIOAuthLoginIPChanged)
			}
		})
	}
}
