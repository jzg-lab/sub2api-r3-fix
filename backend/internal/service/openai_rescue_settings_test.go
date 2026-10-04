package service

// 救治区 settings 集成测试（task 3.8）：容错读矩阵（键缺失/空/坏 JSON/
// 校验不过 → 安全缺省关态）/ Set→Get 往返 / Resolve 矩阵（分钟→Duration、
// 零值回退）/ 校验矩阵。

import (
	"context"
	"errors"
	"testing"
	"time"
)

type rescueLaneSettingsRepoStub struct {
	values map[string]string
	sets   map[string]string
}

func (s *rescueLaneSettingsRepoStub) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (s *rescueLaneSettingsRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (s *rescueLaneSettingsRepoStub) Set(_ context.Context, key, value string) error {
	if s.sets == nil {
		s.sets = map[string]string{}
	}
	s.sets[key] = value
	return nil
}

func (s *rescueLaneSettingsRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}

func (s *rescueLaneSettingsRepoStub) SetMultiple(context.Context, map[string]string) error {
	return nil
}

func (s *rescueLaneSettingsRepoStub) GetAll(context.Context) (map[string]string, error) {
	return nil, nil
}

func (s *rescueLaneSettingsRepoStub) Delete(context.Context, string) error {
	return nil
}

func TestGetOpenAIRescueLaneSettingsHappyPath(t *testing.T) {
	svc := NewSettingService(&rescueLaneSettingsRepoStub{values: map[string]string{
		SettingKeyOpenAIRescueLane: `{"enabled":true,"group_id":99,"consecutive_clean_passes":4,"reconcile_interval_minutes":3}`,
	}}, nil)

	got, err := svc.GetOpenAIRescueLaneSettings(context.Background())
	if err != nil {
		t.Fatalf("GetOpenAIRescueLaneSettings: %v", err)
	}
	if !got.Enabled || got.GroupID != 99 || got.ConsecutiveCleanPasses != 4 || got.ReconcileIntervalMinutes != 3 {
		t.Fatalf("settings=%+v, want enabled/99/4/3", got)
	}
}

func TestGetOpenAIRescueLaneSettingsToleranceMatrix(t *testing.T) {
	cases := []struct {
		name    string
		haveKey bool
		raw     string
	}{
		{"键缺失（部署初始态）", false, ""},
		{"空串", true, ""},
		{"坏 JSON", true, "{bad"},
		{"校验不过（分钟越界）", true, `{"enabled":true,"group_id":1,"reconcile_interval_minutes":99999}`},
		{"校验不过（阈值越界）", true, `{"enabled":true,"consecutive_clean_passes":-1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			values := map[string]string{}
			if tc.haveKey {
				values[SettingKeyOpenAIRescueLane] = tc.raw
			}
			svc := NewSettingService(&rescueLaneSettingsRepoStub{values: values}, nil)
			got, err := svc.GetOpenAIRescueLaneSettings(context.Background())
			if got != DefaultOpenAIRescueLaneSettings() {
				t.Fatalf("settings=%+v err=%v, want safe defaults", got, err)
			}
			if got.Enabled {
				t.Fatalf("tolerance path must never enable the lane")
			}
		})
	}
}

func TestSetOpenAIRescueLaneSettingsRoundtrip(t *testing.T) {
	repo := &rescueLaneSettingsRepoStub{}
	svc := NewSettingService(repo, nil)
	want := OpenAIRescueLaneSettings{Enabled: true, GroupID: 99, ConsecutiveCleanPasses: 8, ReconcileIntervalMinutes: 2}

	if err := svc.SetOpenAIRescueLaneSettings(context.Background(), want); err != nil {
		t.Fatalf("SetOpenAIRescueLaneSettings: %v", err)
	}
	raw, ok := repo.sets[SettingKeyOpenAIRescueLane]
	if !ok {
		t.Fatalf("settings key not written")
	}
	repo.values = map[string]string{SettingKeyOpenAIRescueLane: raw}
	got, err := svc.GetOpenAIRescueLaneSettings(context.Background())
	if err != nil || got != want {
		t.Fatalf("roundtrip=%+v err=%v, want %+v", got, err, want)
	}

	if err := svc.SetOpenAIRescueLaneSettings(context.Background(),
		OpenAIRescueLaneSettings{GroupID: -1}); err == nil {
		t.Fatalf("invalid settings must be rejected on Set")
	}
}

func TestResolveOpenAIRescueLaneConfigMatrix(t *testing.T) {
	cases := []struct {
		name     string
		settings OpenAIRescueLaneSettings
		err      error
		want     OpenAIRescueLaneConfig
	}{
		{"读库出错→强制关态", OpenAIRescueLaneSettings{Enabled: true, GroupID: 99}, errors.New("db down"),
			DefaultOpenAIRescueLaneConfig()},
		{"零值→安全缺省（组未配置）", OpenAIRescueLaneSettings{}, nil,
			OpenAIRescueLaneConfig{Enabled: false, GroupID: 0, ConsecutiveCleanPasses: 3, ReconcileInterval: 5 * time.Minute}},
		{"显式值→分钟转Duration", OpenAIRescueLaneSettings{Enabled: true, GroupID: 99, ConsecutiveCleanPasses: 4, ReconcileIntervalMinutes: 3}, nil,
			OpenAIRescueLaneConfig{Enabled: true, GroupID: 99, ConsecutiveCleanPasses: 4, ReconcileInterval: 3 * time.Minute}},
		{"开关开但组未配→开而不通（配置闸兜底）", OpenAIRescueLaneSettings{Enabled: true}, nil,
			OpenAIRescueLaneConfig{Enabled: true, GroupID: 0, ConsecutiveCleanPasses: 3, ReconcileInterval: 5 * time.Minute}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveOpenAIRescueLaneConfig(tc.settings, tc.err)
			if got != tc.want {
				t.Fatalf("config=%+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestOpenAIRescueLaneSettingsValidate(t *testing.T) {
	if err := DefaultOpenAIRescueLaneSettings().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	for name, bad := range map[string]OpenAIRescueLaneSettings{
		"组 id 负数": {GroupID: -1},
		"阈值负数":    {ConsecutiveCleanPasses: -1},
		"阈值超上限":   {ConsecutiveCleanPasses: 101},
		"分钟负数":    {ReconcileIntervalMinutes: -1},
		"分钟超上限":   {ReconcileIntervalMinutes: 1441},
	} {
		t.Run(name, func(t *testing.T) {
			if err := bad.Validate(); err == nil {
				t.Fatalf("settings=%+v must fail validation", bad)
			}
		})
	}
}
