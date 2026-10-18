package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/antibypass"
)

type AntiBypassPolicy = antibypass.Policy

// GetAntiBypassSettings reads switch and budget together so HTTP and live WS
// consumers cannot combine different settings generations.
func (s *SettingService) GetAntiBypassSettings(ctx context.Context) (bool, antibypass.Config, error) {
	if s == nil || s.settingRepo == nil {
		return false, antibypass.Config{}, errors.New("anti-bypass setting repository is unavailable")
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{SettingKeyAntiBypassEnabled, SettingKeyAntiBypassPolicy})
	if err != nil {
		return false, antibypass.Config{}, fmt.Errorf("get anti-bypass policy: %w", err)
	}
	enabled := strings.TrimSpace(values[SettingKeyAntiBypassEnabled]) == "true"
	if !enabled {
		return false, antibypass.DefaultConfig(), nil
	}
	policy, err := antibypass.ParsePolicy(values[SettingKeyAntiBypassPolicy])
	if err != nil {
		return false, antibypass.Config{}, err
	}
	return true, policy.Config(), nil
}

// IsAntiBypassEnabled reads the dedicated gateway security switch. A missing
// legacy setting is treated as false so existing deployments remain unchanged.
func (s *SettingService) IsAntiBypassEnabled(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, errors.New("anti-bypass setting repository is unavailable")
	}
	value, err := s.settingRepo.GetValue(ctx, SettingKeyAntiBypassEnabled)
	if errors.Is(err, ErrSettingNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get anti-bypass setting: %w", err)
	}
	return strings.TrimSpace(value) == "true", nil
}
