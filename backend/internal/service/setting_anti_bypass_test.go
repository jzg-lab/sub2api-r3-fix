package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/antibypass"
	"github.com/stretchr/testify/require"
)

func TestIsAntiBypassEnabledFailsClosedWithoutSettingRepository(t *testing.T) {
	var service *SettingService

	enabled, err := service.IsAntiBypassEnabled(context.Background())

	require.False(t, enabled)
	require.Error(t, err)
}

func TestAntiBypassPolicySettingsFailClosedAndPreserveDefaults(t *testing.T) {
	repo := &panelRateLimitSettingRepo{values: map[string]string{SettingKeyAntiBypassEnabled: "true"}}
	s := &SettingService{settingRepo: repo}
	enabled, config, err := s.GetAntiBypassSettings(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, antibypass.DefaultConfig(), config)
	policy := antibypass.DefaultPolicy()
	policy.MaxConcurrent = 64
	raw, err := json.Marshal(policy)
	require.NoError(t, err)
	repo.values[SettingKeyAntiBypassPolicy] = string(raw)
	enabled, config, err = s.GetAntiBypassSettings(context.Background())
	require.NoError(t, err)
	require.True(t, enabled)
	require.Equal(t, 64, config.MaxConcurrent)
	repo.values[SettingKeyAntiBypassPolicy] = "{}"
	enabled, _, err = s.GetAntiBypassSettings(context.Background())
	require.Error(t, err)
	require.False(t, enabled)
	repo.values[SettingKeyAntiBypassEnabled] = "false"
	enabled, _, err = s.GetAntiBypassSettings(context.Background())
	require.NoError(t, err)
	require.False(t, enabled)
}
