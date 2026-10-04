package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SettingKeyOpenAIRescueLane 救治区配置的 settings 键（单 JSON 块，与
// openai_operations 同一存放形态）。编排器每轮清扫、每次判死提交都重读
// 本键（读通道无缓存），改库即热生效——生产 flip 不需要重启进程。
const SettingKeyOpenAIRescueLane = "openai_rescue_lane"

// OpenAIRescueLaneSettings 救治区可运维配置（JSON 存储，面向运维面板）。
// 零值字段在 Resolve 阶段回退安全缺省（组未配置=闸拒绝入区，阈值 6，
// 对账 5min），保证「键缺失/半配」都落在关态安全侧。
type OpenAIRescueLaneSettings struct {
	Enabled                  bool  `json:"enabled"`
	GroupID                  int64 `json:"group_id"`
	ConsecutiveCleanPasses   int   `json:"consecutive_clean_passes"`
	ReconcileIntervalMinutes int   `json:"reconcile_interval_minutes"`
}

func DefaultOpenAIRescueLaneSettings() OpenAIRescueLaneSettings {
	return OpenAIRescueLaneSettings{
		Enabled:                  false,
		GroupID:                  0,
		ConsecutiveCleanPasses:   openAIRescueDefaultCleanPasses,
		ReconcileIntervalMinutes: int(openAIRescueDefaultReconcileInterval / time.Minute),
	}
}

func (v OpenAIRescueLaneSettings) Validate() error {
	if v.GroupID < 0 {
		return fmt.Errorf("group_id must be nonnegative")
	}
	if v.ConsecutiveCleanPasses < 0 || v.ConsecutiveCleanPasses > 100 {
		return fmt.Errorf("consecutive_clean_passes must be between 0 and 100")
	}
	if v.ReconcileIntervalMinutes < 0 || v.ReconcileIntervalMinutes > 1440 {
		return fmt.Errorf("reconcile_interval_minutes must be between 0 and 1440")
	}
	return nil
}

// GetOpenAIRescueLaneSettings 容错读：键缺失/空/坏 JSON/校验不过 → 安全
// 缺省（关态），与 openai_operations 同一容错语义。
func (s *SettingService) GetOpenAIRescueLaneSettings(ctx context.Context) (OpenAIRescueLaneSettings, error) {
	defaults := DefaultOpenAIRescueLaneSettings()
	if s == nil || s.settingRepo == nil {
		return defaults, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIRescueLane)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && strings.TrimSpace(raw) == "") {
		return defaults, nil
	}
	if err != nil {
		return defaults, err
	}
	if err := json.Unmarshal([]byte(raw), &defaults); err != nil {
		return DefaultOpenAIRescueLaneSettings(), fmt.Errorf("invalid stored rescue lane settings")
	}
	if err := defaults.Validate(); err != nil {
		return DefaultOpenAIRescueLaneSettings(), err
	}
	return defaults, nil
}

// SetOpenAIRescueLaneSettings 校验后落库（运维面板/CLI 通道）。
func (s *SettingService) SetOpenAIRescueLaneSettings(ctx context.Context, value OpenAIRescueLaneSettings) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("settings repository unavailable")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, SettingKeyOpenAIRescueLane, string(raw))
}

// ResolveOpenAIRescueLaneConfig settings → 编排器配置（分钟 → Duration，
// 零值回退缺省）。getter 出错时自带缺省返回，此处再按 err 强制关态。
func ResolveOpenAIRescueLaneConfig(settings OpenAIRescueLaneSettings, err error) OpenAIRescueLaneConfig {
	if err != nil {
		return DefaultOpenAIRescueLaneConfig()
	}
	cfg := DefaultOpenAIRescueLaneConfig()
	cfg.Enabled = settings.Enabled
	if settings.GroupID > 0 {
		cfg.GroupID = settings.GroupID
	}
	if settings.ConsecutiveCleanPasses > 0 {
		cfg.ConsecutiveCleanPasses = settings.ConsecutiveCleanPasses
	}
	if settings.ReconcileIntervalMinutes > 0 {
		cfg.ReconcileInterval = time.Duration(settings.ReconcileIntervalMinutes) * time.Minute
	}
	return cfg
}
