package service

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManagedDeploymentServiceDisabledAndUnavailable(t *testing.T) {
	s := NewManagedDeploymentService("")
	result, err := s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.False(t, result.Enabled)
	_, err = s.Command(context.Background(), "prepare", "release-1")
	require.ErrorIs(t, err, ErrManagedDeploymentUnavailable)
	s = NewManagedDeploymentService("relative.sock")
	require.True(t, s.Enabled(), "a broken configuration must still block the legacy updater")
	_, err = s.Snapshot(context.Background(), false)
	require.ErrorIs(t, err, ErrManagedDeploymentUnavailable)
}

func TestManagedDeploymentServiceUnixProtocol(t *testing.T) {
	dir, err := os.MkdirTemp("", "deploy-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "deployment", r.Host)
		require.Empty(t, r.Header.Get("Authorization"))
		if r.Method == "POST" {
			body, readErr := io.ReadAll(r.Body)
			require.NoError(t, readErr)
			require.JSONEq(t, `{"id":"release-1"}`, string(body))
			require.Equal(t, "/prepare", r.URL.Path)
		} else {
			_, _ = io.WriteString(w, `{"enabled":true,"candidates":[],"job":{"mode":"handoff","live_handoff":true,"id":"job-2","candidate_id":"release-2","phase":"ACCEPTED","previous_source":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","previous_version":"v0.2.17+r17.1","actions":["rollback"]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":true,"candidates":[],"job":{"id":"job-1","candidate_id":"release-1","phase":"PREPARATION_FAILED","reused":true,"actions":[]},"rollback_job":{"id":"job-old","previous_version":"v0.2.17+r17.1","phase":"ACCEPTED","actions":["rollback"]}}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	s := NewManagedDeploymentService(socket)
	result, err := s.Command(context.Background(), "prepare", "release-1")
	require.NoError(t, err)
	require.Equal(t, "PREPARATION_FAILED", result.Job.Phase)
	require.True(t, result.Job.Reused)
	require.False(t, result.Job.LiveHandoff, "legacy responses must not claim live handoff")
	require.Equal(t, "job-old", result.RollbackJob.ID)
	require.Equal(t, "v0.2.17+r17.1", result.RollbackJob.PreviousVersion)
	require.Equal(t, []string{"rollback"}, result.RollbackJob.Actions)
	result, err = s.Snapshot(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "handoff", result.Job.Mode)
	require.True(t, result.Job.LiveHandoff)
	require.Equal(t, "ACCEPTED", result.Job.Phase)
	require.Equal(t, strings.Repeat("a", 40), result.Job.PreviousSource)
	require.Equal(t, "v0.2.17+r17.1", result.Job.PreviousVersion)
	require.Equal(t, []string{"rollback"}, result.Job.Actions)
	require.Nil(t, result.RollbackJob)
	for _, id := range []string{"../secret", "a b", "", strings.Repeat("a", 65)} {
		_, err = s.Command(context.Background(), "prepare", id)
		require.Error(t, err)
	}
	_, err = s.Command(context.Background(), "shell", "release-1")
	require.Error(t, err)
}
