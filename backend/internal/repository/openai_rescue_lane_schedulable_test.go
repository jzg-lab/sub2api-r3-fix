package repository

// 救治区专用调度闸（r17ax）：没过资格考（Extra 无合格戳）的判死号在
// 「仅绑救治组」拓扑下放行开调度；多绑一个组即拒绝（闸的客户保护语义
// 不能被绕过）；无代理直连账号也可进入救治区。

import (
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const openAIRescueAccountLockPattern = `(?s)SELECT id, platform, type, credentials, extra, proxy_id, parent_account_id.*FROM accounts.*FOR NO KEY UPDATE`

func expectRescueAccountLock(mock sqlmock.Sqlmock, platform, accType string, proxyID any) {
	mock.ExpectQuery(openAIRescueAccountLockPattern).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "platform", "type", "credentials", "extra", "proxy_id", "parent_account_id"},
		).AddRow(
			int64(42),
			platform,
			accType,
			[]byte(`{"email":"user@example.com"}`),
			[]byte(`{}`),
			proxyID,
			nil,
		))
}

func expectRescueGroupBindings(mock sqlmock.Sqlmock, groupIDs ...int64) {
	rows := sqlmock.NewRows([]string{"group_id"})
	for _, id := range groupIDs {
		rows.AddRow(id)
	}
	mock.ExpectQuery(`SELECT group_id FROM account_groups WHERE account_id = \$1`).
		WithArgs(int64(42)).
		WillReturnRows(rows)
}

func TestValidateOpenAIOAuthSchedulableInRescueLaneMatrix(t *testing.T) {
	tests := []struct {
		name      string
		platform  string
		accType   string
		proxyID   any
		groups    []int64
		wantError error
	}{
		{
			name:      "仅绑救治组+有代理→放行（没合格戳也不 409）",
			platform:  "openai",
			accType:   "oauth",
			proxyID:   int64(7),
			groups:    []int64{99},
			wantError: nil,
		},
		{
			name:      "救治组外多绑原池组→拒绝（闸语义不可绕过）",
			platform:  "openai",
			accType:   "oauth",
			proxyID:   int64(7),
			groups:    []int64{99, 3},
			wantError: service.ErrOpenAIOAuthRescueBindingRequired,
		},
		{
			name:      "零绑定（半进区未改绑）→拒绝",
			platform:  "openai",
			accType:   "oauth",
			proxyID:   int64(7),
			groups:    nil,
			wantError: service.ErrOpenAIOAuthRescueBindingRequired,
		},
		{
			name:      "无代理→允许直连救治",
			platform:  "openai",
			accType:   "oauth",
			proxyID:   nil,
			groups:    []int64{99},
			wantError: nil,
		},
		{
			name:      "非浏览器 OAuth 号→不适用（走普通通道语义）",
			platform:  "claude",
			accType:   "oauth",
			proxyID:   int64(7),
			groups:    nil,
			wantError: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := openAIOAuthPrepareMock(t)
			expectRescueAccountLock(mock, tt.platform, tt.accType, tt.proxyID)
			// 浏览器 OAuth 号：先校验组绑定，再校验已配置的代理。
			if tt.platform == "openai" {
				expectRescueGroupBindings(mock, tt.groups...)
				if len(tt.groups) == 1 && tt.groups[0] == 99 && tt.proxyID != nil {
					mock.ExpectQuery(`SELECT EXISTS`).WithArgs(int64(7), service.StatusActive).
						WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
				}
			}

			err := validateOpenAIOAuthSchedulableInRescueLane(
				t.Context(), db, 42, 99, true)

			if tt.wantError == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantError)
			}
		})
	}
}

func TestValidateOpenAIOAuthSchedulableInRescueLaneSkipsOnDisable(t *testing.T) {
	db, _ := openAIOAuthPrepareMock(t)

	// 关调度不过闸（判死号出区撤调不受拓扑限制）。
	err := validateOpenAIOAuthSchedulableInRescueLane(t.Context(), db, 42, 99, false)

	require.NoError(t, err)
}

func TestValidateOpenAIOAuthSchedulableInRescueLaneRejectsMissingGroupConfig(t *testing.T) {
	db, mock := openAIOAuthPrepareMock(t)
	expectRescueAccountLock(mock, "openai", "oauth", int64(7))

	err := validateOpenAIOAuthSchedulableInRescueLane(t.Context(), db, 42, 0, true)

	require.ErrorIs(t, err, service.ErrOpenAIOAuthRescueBindingRequired)
}
