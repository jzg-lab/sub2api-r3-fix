package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

func TestVerifiedCodexTransportMatchesNativeCapture(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/client-identity/" + codexCLIVersion + ".json")
	require.NoError(t, err)
	var capture struct {
		Version string                 `json:"version"`
		TLS     tlsfingerprint.Profile `json:"tls"`
	}
	require.NoError(t, json.Unmarshal(raw, &capture))
	require.Equal(t, codexCLIVersion, capture.Version)
	actual := verifiedCodexTLSProfile()
	require.NoError(t, actual.Validate())
	capture.TLS.Name = actual.Name
	require.Equal(t, &capture.TLS, actual)
	actual.CipherSuites[0] = 0
	require.NotEqual(t, actual, verifiedCodexTLSProfile(), "callers cannot mutate the next request's profile")
}

func TestCodexSyncCannotAdoptAnUncapturedRelease(t *testing.T) {
	repo := newCodexVersionSyncSettingRepoStub(map[string]string{
		SettingKeyOpenAICodexClientVersionSynced: codexCLIVersion,
	})
	upstream := &codexVersionSyncGitHubStub{latest: &GitHubRelease{TagName: "rust-v99.0.0"}}
	newCodexVersionSyncService(repo, upstream).runOnce()
	require.Empty(t, repo.syncedWrites())
	svc := NewSettingService(&codexVersionSettingRepoStub{values: map[string]string{
		SettingKeyOpenAICodexClientVersion: "99.0.0", SettingKeyOpenAICodexClientVersionSynced: "99.0.0",
	}}, nil)
	require.Equal(t, codexCLIVersion, svc.GetOpenAICodexClientVersion(t.Context()))
	require.False(t, IsVerifiedCodexClientVersion("99.0.0"))
}
