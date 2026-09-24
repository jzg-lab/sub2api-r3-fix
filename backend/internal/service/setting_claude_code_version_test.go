//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestGetClaudeCodeClientVersionPriority(t *testing.T) {
	for _, tt := range []struct {
		name   string
		manual string
		synced string
		want   string
	}{
		{name: "manual wins", manual: "2.1.280", synced: "2.1.281", want: "2.1.280"},
		{name: "manual normalized", manual: " v2.1.280 ", synced: "2.1.281", want: "2.1.280"},
		{name: "synced fallback", synced: "2.1.281", want: "2.1.281"},
		{name: "invalid manual falls through", manual: "invalid", synced: "2.1.281", want: "2.1.281"},
		{name: "invalid synced uses builtin", synced: "2.1.9", want: claude.CLIVersion()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &authSourceDefaultsRepoStub{values: map[string]string{
				SettingKeyClaudeCodeClientVersion:       tt.manual,
				SettingKeyClaudeCodeClientVersionSynced: tt.synced,
			}}
			svc := NewSettingService(repo, &config.Config{})
			require.Equal(t, tt.want, svc.GetClaudeCodeClientVersion(context.Background()))
		})
	}
}

func TestLatestClaudeCodeStableReleaseVersion(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v2.1.279"},
		{TagName: "v2.1.281"},
		{TagName: "v2.1.282-beta.1", Prerelease: true},
		{TagName: "2.1.999"},
		{TagName: "v2.1.300", Draft: true},
	}
	require.Equal(t, "2.1.281", latestClaudeCodeStableReleaseVersion(releases))
}
