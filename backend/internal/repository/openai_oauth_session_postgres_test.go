package repository

import (
	"errors"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/migrate"
	"github.com/Wei-Shaw/sub2api/ent/pendingauthsession"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newOpenAIOAuthPostgresSessionStores(t *testing.T) (*dbent.Client, func() service.OpenAIOAuthSessionStore) {
	t.Helper()
	db := newProbePostgresWithMigrations(t, nil)
	driver := entsql.OpenDB(dialect.Postgres, db)
	require.NoError(t, migrate.Create(t.Context(), migrate.NewSchema(driver),
		[]*schema.Table{migrate.UsersTable, migrate.PendingAuthSessionsTable}, migrate.WithForeignKeys(false)))
	client := dbent.NewClient(dbent.Driver(driver))
	return client, func() service.OpenAIOAuthSessionStore {
		// No shared in-memory session cache: every instance must read the database.
		freshClient := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		return service.NewPendingAuthOpenAIOAuthSessionStore(service.NewAuthPendingIdentityService(freshClient))
	}
}

func postgresBrowserSession(kind string) *service.OpenAIOAuthSession {
	session := &service.OpenAIOAuthSession{
		ID:             strings.Repeat("a", 32),
		ClientID:       "fixture-client",
		RedirectURI:    "http://localhost:1455/auth/callback",
		ProxyID:        15,
		ProxyRouteHash: strings.Repeat("b", 64),
		Platform:       service.PlatformOpenAI,
		CreatedAt:      time.Now().UTC(),
	}
	browserID := strings.Repeat("c", 32)
	if strings.HasPrefix(kind, "reauthorization") {
		session.ReauthorizationAccountID = 1207
		session.ReauthorizationRevision = session.CreatedAt.Add(-time.Hour).Format(time.RFC3339Nano)
		session.ReauthorizationExitIP = "198.51.100.25"
		session.ReauthorizationBrowserSessionID = browserID
		if strings.HasSuffix(kind, "proof") {
			session.ReauthorizationCredentialsHash = strings.Repeat("d", 64)
		}
	} else {
		session.LoginExitIP = "198.51.100.25"
		session.LoginBrowserSessionID = browserID
		if strings.HasSuffix(kind, "proof") {
			session.LoginCredentialsHash = strings.Repeat("d", 64)
		}
	}
	return session
}

func TestOpenAIOAuthSessionPostgresDurableBindingsAndSingleConsumption(t *testing.T) {
	for _, kind := range []string{"login browser", "login proof", "reauthorization browser", "reauthorization proof"} {
		t.Run(kind, func(t *testing.T) {
			client, newStore := newOpenAIOAuthPostgresSessionStores(t)
			expected := postgresBrowserSession(kind)
			require.NoError(t, newStore().Create(t.Context(), expected))
			loaded, err := newStore().Get(t.Context(), expected.ID)
			require.NoError(t, err)
			require.Equal(t, *expected, *loaded)

			replacement := *expected
			replacement.ProxyID++
			require.Error(t, newStore().Create(t.Context(), &replacement))
			loaded, err = newStore().Get(t.Context(), expected.ID)
			require.NoError(t, err)
			require.Equal(t, *expected, *loaded)

			type result struct {
				session *service.OpenAIOAuthSession
				err     error
			}
			const consumers = 8
			results := make(chan result, consumers)
			start := make(chan struct{})
			for range consumers {
				store := newStore()
				go func() {
					<-start
					session, err := store.Consume(t.Context(), expected.ID)
					results <- result{session, err}
				}()
			}
			close(start)
			var successes, replays int
			for range consumers {
				result := <-results
				switch {
				case result.err == nil:
					successes++
					require.NotNil(t, result.session)
					require.Equal(t, *expected, *result.session)
				case errors.Is(result.err, service.ErrPendingAuthSessionConsumed):
					replays++
					require.Nil(t, result.session)
				default:
					require.NoError(t, result.err)
				}
			}
			require.Equal(t, 1, successes)
			require.Equal(t, consumers-1, replays)
			_, err = newStore().Get(t.Context(), expected.ID)
			require.ErrorIs(t, err, service.ErrPendingAuthSessionConsumed)
			_, err = newStore().Consume(t.Context(), expected.ID)
			require.ErrorIs(t, err, service.ErrPendingAuthSessionConsumed)
			stored, err := client.PendingAuthSession.Query().
				Where(pendingauthsession.SessionTokenEQ(expected.ID)).Only(t.Context())
			require.NoError(t, err)
			require.NotNil(t, stored.ConsumedAt)
		})
	}
}

func TestOpenAIOAuthSessionPostgresRejectsExpiredOrMalformedBindings(t *testing.T) {
	for _, mutation := range []string{"expired", "browser type", "missing browser", "mixed flow", "missing IP"} {
		t.Run(mutation, func(t *testing.T) {
			client, newStore := newOpenAIOAuthPostgresSessionStores(t)
			expected := postgresBrowserSession("reauthorization proof")
			if mutation == "expired" {
				expected.CreatedAt = expected.CreatedAt.Add(-3 * time.Hour)
			}
			require.NoError(t, newStore().Create(t.Context(), expected))
			wantErr := service.ErrOpenAIOAuthSessionInvalid
			if mutation == "expired" {
				wantErr = service.ErrPendingAuthSessionExpired
			} else {
				stored, err := client.PendingAuthSession.Query().
					Where(pendingauthsession.SessionTokenEQ(expected.ID)).Only(t.Context())
				require.NoError(t, err)
				payload := stored.LocalFlowState["openai_oauth"].(map[string]any)
				switch mutation {
				case "browser type":
					payload["reauthorization_browser_session_id"] = 123
				case "missing browser":
					delete(payload, "reauthorization_browser_session_id")
				case "mixed flow":
					payload["login_exit_ip"] = "198.51.100.25"
				case "missing IP":
					delete(payload, "reauthorization_exit_ip")
				}
				_, err = stored.Update().SetLocalFlowState(stored.LocalFlowState).Save(t.Context())
				require.NoError(t, err)
			}
			loaded, err := newStore().Get(t.Context(), expected.ID)
			require.ErrorIs(t, err, wantErr)
			require.Nil(t, loaded)
			consumed, err := newStore().Consume(t.Context(), expected.ID)
			require.ErrorIs(t, err, wantErr)
			require.Nil(t, consumed)
		})
	}
}
