//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
	latestRepo     string
	recentRepo     string
	latestCalls    int
	recentCalls    int
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.latestRepo = repo
	s.latestCalls++
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	s.recentRepo = repo
	s.recentCalls++
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}

func TestUpdateServiceUsesProjectRepository(t *testing.T) {
	client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.2.17"}}
	svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.16", "release")
	_, err := svc.CheckUpdate(context.Background(), true)
	require.NoError(t, err)
	require.Equal(t, "jzg-lab/sub2api-r3-fix", client.latestRepo)
	_, err = svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Equal(t, "jzg-lab/sub2api-r3-fix", client.recentRepo)
}

func TestUpdateServiceRejectsForeignOrUnboundCache(t *testing.T) {
	for _, repo := range []string{"", "Wei-Shaw/sub2api"} {
		t.Run(repo, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"repository":   repo,
				"latest":       "9.9.9",
				"timestamp":    time.Now().Unix(),
				"release_info": &ReleaseInfo{HTMLURL: "https://github.com/Wei-Shaw/sub2api/releases/tag/v9.9.9"},
			})
			require.NoError(t, err)
			client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "v0.2.17"}}
			svc := NewUpdateService(&updateServiceCacheStub{data: string(data)}, client, "0.2.16", "release")
			info, err := svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.Equal(t, "0.2.17", info.LatestVersion)
			require.Equal(t, 1, client.latestCalls)
			info, err = svc.CheckUpdate(context.Background(), false)
			require.NoError(t, err)
			require.True(t, info.Cached)
			require.Equal(t, 1, client.latestCalls)
		})
	}
}

func TestUpdateServiceRejectsInPlaceOperationsForSourceBuilds(t *testing.T) {
	for _, buildType := range []string{"source", "", "unknown"} {
		t.Run(buildType, func(t *testing.T) {
			client := &updateServiceGitHubClientStub{
				release:        &GitHubRelease{TagName: "v0.2.18"},
				recentReleases: []*GitHubRelease{{TagName: "v0.2.16"}},
			}
			svc := NewUpdateService(&updateServiceCacheStub{}, client, "0.2.17", buildType)
			require.ErrorContains(t, svc.PerformUpdate(context.Background()), "deployment workflow")
			require.ErrorContains(t, svc.RollbackToVersion(context.Background(), "0.2.16"), "deployment workflow")
			require.ErrorContains(t, svc.Rollback(), "deployment workflow")
			_, err := svc.ListRollbackVersions(context.Background())
			require.ErrorContains(t, err, "deployment workflow")
			require.Zero(t, client.latestCalls)
			require.Zero(t, client.recentCalls)
		})
	}
}

func TestUpdateServiceVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		name, current, latest string
		update, warning       bool
	}{
		{"sequence", "0.2.17+r17.1", "v0.2.17+r17.2", true, false},
		{"numeric sequence", "0.2.17+r17.9", "v0.2.17+r17.10", true, false},
		{"global sequence", "0.2.18+r17.9", "v0.2.17+r17.10", true, false},
		{"old sequence", "0.2.17+r17.10", "v0.2.18+r17.9", false, false},
		{"same artifact version", "0.2.17+r17.2.fast.0123456789ab", "v0.2.17+r17.2", false, false},
		{"legacy metadata patch", "0.2.17+r17bi", "v0.2.16", false, false},
		{"prerelease", "0.2.17-rc.1", "v0.2.17", true, false},
		{"legacy transition", "0.3.0+r17bi.fast.0123456789ab", "v0.2.17+r17.1", false, true},
		{"reverse transition", "0.2.17+r17.1", "v0.3.0", false, true},
		{"invalid sequence", "0.2.17+r17.1", "v0.2.17+r17.0", false, true},
		{"leading zero", "0.2.17+r17.1", "v0.2.17+r17.02", false, true},
		{"overflow", "0.2.17+r17.1", "v0.2.17+r17.18446744073709551616", false, true},
		{"conflicting baseline", "0.2.17+r17.2", "v0.2.18+r17.2", false, true},
		{"unknown metadata", "0.2.17+r17.1", "v0.2.17+r17.2.other", false, true},
		{"invalid artifact hash", "0.2.17+r17.1", "v0.2.17+r17.2.fast.zzzzzzzzzzzz", false, true},
		{"invalid version", "dev", "v0.2.17", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: tc.latest}}
			svc := NewUpdateService(&updateServiceCacheStub{}, client, tc.current, "release")
			info, err := svc.CheckUpdate(context.Background(), true)
			require.NoError(t, err)
			require.Equal(t, tc.update, info.HasUpdate)
			if tc.warning {
				require.NotEmpty(t, info.Warning)
			} else {
				require.Empty(t, info.Warning)
			}
		})
	}
}

func TestUpdateServiceUncertainVersionCannotUpdate(t *testing.T) {
	for _, release := range []*GitHubRelease{nil, {TagName: "v0.2.17+r17.1"}} {
		svc := NewUpdateService(&updateServiceCacheStub{}, &updateServiceGitHubClientStub{release: release}, "0.3.0+r17bi", "release")
		err := svc.PerformUpdate(context.Background())
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoUpdateAvailable)
		require.Contains(t, err.Error(), "update check failed")
	}
}

func TestUpdateServiceProjectRollbackOrderingRejectsUnknownIdentity(t *testing.T) {
	svc := newRollbackTestService("0.2.17+r17.12", []*GitHubRelease{
		{TagName: "v0.2.17+r17.2"},
		{TagName: "v0.2.17+r17.10"},
		{TagName: "v0.2.16+r17.11"},
		{TagName: "v0.2.17+r17.12"},
		{TagName: "v0.2.17+r17.02"},
		{TagName: "v0.2.16"},
		{TagName: "not-a-version"},
	})
	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.2.16+r17.11", versions[0].Version)
	require.Equal(t, "0.2.17+r17.10", versions[1].Version)
	require.Equal(t, "0.2.17+r17.2", versions[2].Version)
}
