package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestCreateAccountRequestSchedulingDefaults(t *testing.T) {
	for _, tc := range []struct {
		payload  string
		priority int
	}{
		{`{}`, 2},
		{`{"priority":null}`, 2},
		{`{"priority":0}`, 0},
		{`{"priority":17}`, 17},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			var request CreateAccountRequest
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &request))
			require.Equal(t, tc.priority, request.priorityValue())
		})
	}
	require.Equal(t, 5, service.LocalAccountConcurrency)
	require.Equal(t, 2, service.LocalAccountPriority)
}

func TestCodexImportSchedulingDefaultsAndOverrides(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			adminService := newCodexImportMemoryAdminService(nil)
			handler := &AccountHandler{adminService: adminService}
			req := CodexSessionImportRequest{Content: buildCodexImportTestJWT(t, time.Now().Add(time.Hour), map[string]any{"sub": "fixture-user"})}
			concurrency, priority := 75, 0
			if explicit {
				req.Concurrency, req.Priority = &concurrency, &priority
			}
			entries, err := parseCodexSessionImportEntries(req)
			require.NoError(t, err)
			result, err := handler.importCodexSessions(context.Background(), req, entries)
			require.NoError(t, err)
			require.Equal(t, 1, result.Created, "%+v", result)
			require.Len(t, adminService.createdAccounts, 1)
			wantConcurrency, wantPriority := 5, 2
			if explicit {
				wantConcurrency, wantPriority = concurrency, priority
			}
			require.Equal(t, wantConcurrency, adminService.createdAccounts[0].Concurrency)
			require.Equal(t, wantPriority, adminService.createdAccounts[0].Priority)
		})
	}
}
