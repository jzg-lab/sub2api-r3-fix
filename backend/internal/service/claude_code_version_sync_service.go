package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	claudeCodeVersionSyncInterval = time.Hour
	claudeCodeVersionSyncTimeout  = 30 * time.Second
	claudeCodeVersionSyncRepo     = "anthropics/claude-code"
	claudeCodeVersionSyncPerPage  = 30
	claudeCodeVersionTagPrefix    = "v"
)

// ClaudeCodeVersionSyncService keeps the outbound Claude Code identity current
// without requiring a Sub2API release for every upstream CLI release.
type ClaudeCodeVersionSyncService struct {
	settingRepo    SettingRepository
	settingService *SettingService
	githubClient   GitHubReleaseClient
	interval       time.Duration
	stopCh         chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

func NewClaudeCodeVersionSyncService(
	settingRepo SettingRepository,
	settingService *SettingService,
	githubClient GitHubReleaseClient,
	interval time.Duration,
) *ClaudeCodeVersionSyncService {
	return &ClaudeCodeVersionSyncService{
		settingRepo:    settingRepo,
		settingService: settingService,
		githubClient:   githubClient,
		interval:       interval,
		stopCh:         make(chan struct{}),
	}
}

func (s *ClaudeCodeVersionSyncService) Start() {
	if s == nil || s.settingRepo == nil || s.githubClient == nil || s.interval <= 0 {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()

		s.runInitial()
		for {
			select {
			case <-ticker.C:
				s.runOnce()
			case <-s.stopCh:
				return
			}
		}
	}()
}

func (s *ClaudeCodeVersionSyncService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}

func (s *ClaudeCodeVersionSyncService) runInitial() {
	if s.syncedWithinInterval() {
		return
	}
	s.runOnce()
}

func (s *ClaudeCodeVersionSyncService) syncedWithinInterval() bool {
	if s.interval <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), claudeCodeVersionSyncTimeout)
	defer cancel()

	setting, err := s.settingRepo.Get(ctx, SettingKeyClaudeCodeClientVersionSynced)
	if err != nil || setting == nil || setting.UpdatedAt.IsZero() {
		return false
	}
	if NormalizeClaudeCodeClientVersion(setting.Value) == "" {
		return false
	}
	return time.Since(setting.UpdatedAt) < s.interval
}

func (s *ClaudeCodeVersionSyncService) runOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), claudeCodeVersionSyncTimeout)
	defer cancel()

	if !s.autoSyncEnabled(ctx) {
		return
	}
	latest := s.fetchLatestStableVersion(ctx)
	if latest == "" {
		return
	}

	current := NormalizeClaudeCodeClientVersion(s.currentSyncedVersion(ctx))
	if current != "" && CompareVersions(latest, current) <= 0 {
		return
	}
	if err := s.settingRepo.Set(ctx, SettingKeyClaudeCodeClientVersionSynced, latest); err != nil {
		slog.Warn("claude_code_version_sync_persist_failed", "version", latest, "error", err)
		return
	}
	if s.settingService != nil {
		s.settingService.InvalidateClaudeCodeClientVersionCache()
	}
	slog.Info("claude_code_version_synced", "previous", current, "version", latest)
}

func (s *ClaudeCodeVersionSyncService) fetchLatestStableVersion(ctx context.Context) string {
	release, err := s.githubClient.FetchLatestRelease(ctx, claudeCodeVersionSyncRepo)
	if err != nil {
		slog.Warn("claude_code_version_sync_latest_fetch_failed", "error", err)
	} else if version := latestClaudeCodeStableReleaseVersion([]*GitHubRelease{release}); version != "" {
		return version
	}

	releases, err := s.githubClient.FetchRecentReleases(ctx, claudeCodeVersionSyncRepo, claudeCodeVersionSyncPerPage)
	if err != nil {
		slog.Warn("claude_code_version_sync_fetch_failed", "error", err)
		return ""
	}
	version := latestClaudeCodeStableReleaseVersion(releases)
	if version == "" {
		slog.Warn("claude_code_version_sync_no_stable_release", "repo", claudeCodeVersionSyncRepo)
	}
	return version
}

func (s *ClaudeCodeVersionSyncService) autoSyncEnabled(ctx context.Context) bool {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyClaudeCodeVersionAutoSyncEnabled)
	if err != nil || strings.TrimSpace(value) == "" {
		return true
	}
	return strings.TrimSpace(value) == "true"
}

func (s *ClaudeCodeVersionSyncService) currentSyncedVersion(ctx context.Context) string {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyClaudeCodeClientVersionSynced)
	if err != nil {
		return ""
	}
	return value
}

func latestClaudeCodeStableReleaseVersion(releases []*GitHubRelease) string {
	best := ""
	for _, release := range releases {
		if release == nil || release.Draft || release.Prerelease {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if !strings.HasPrefix(tag, claudeCodeVersionTagPrefix) {
			continue
		}
		version := NormalizeClaudeCodeClientVersion(strings.TrimPrefix(tag, claudeCodeVersionTagPrefix))
		if version == "" || strings.Contains(version, "-") {
			continue
		}
		if best == "" || CompareVersions(version, best) > 0 {
			best = version
		}
	}
	return best
}
