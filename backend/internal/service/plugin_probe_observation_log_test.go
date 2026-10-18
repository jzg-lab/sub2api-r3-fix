package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPluginProbeObservationLogAllowlist(t *testing.T) {
	valid := func() map[string]any {
		return map[string]any{
			"msg": "quality_probe_observation", "account_id": 42,
			"template_generation": 2, "cookie_generation": 0,
			"probe": 3, "quality_rerolls": 1, "verdict": "pass",
			"action": "scheduled", "reasoning_tokens": 1400,
			"observed_at":     "2026-10-13T04:00:00+08:00",
			"untrusted_extra": "fixture-private-content-must-be-dropped",
		}
	}
	for name, change := range map[string]func(map[string]any){
		"valid":           func(map[string]any) {},
		"invalid_account": func(v map[string]any) { v["account_id"] = 0 },
		"unknown_verdict": func(v map[string]any) { v["verdict"] = "unbounded-content" },
		"unknown_action":  func(v map[string]any) { v["action"] = "unbounded-content" },
		"invalid_time":    func(v map[string]any) { v["observed_at"] = "unbounded-content" },
		"negative_usage":  func(v map[string]any) { v["reasoning_tokens"] = -1 },
		"negative_probe":  func(v map[string]any) { v["probe"] = -1 },
		"invalid_reroll":  func(v map[string]any) { v["quality_rerolls"] = 4 },
		"no_generation":   func(v map[string]any) { v["template_generation"] = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			event := valid()
			change(event)
			raw, err := json.Marshal(event)
			require.NoError(t, err)
			raw = append(raw, '\n')
			var output bytes.Buffer
			writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			// Stderr can split a single event across arbitrary write boundaries.
			for _, b := range raw {
				_, err := writer.Write([]byte{b})
				require.NoError(t, err)
			}
			if name != "valid" {
				require.Empty(t, output.String())
				return
			}
			var logged map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &logged))
			require.Equal(t, "quality_probe_observation", logged["msg"])
			require.EqualValues(t, 42, logged["account_id"])
			require.EqualValues(t, 1400, logged["reasoning_tokens"])
			require.NotContains(t, output.String(), "fixture-private-content")
			require.NotContains(t, logged, "untrusted_extra")
		})
	}
}

func TestPluginBusinessUnknownLogAllowlist(t *testing.T) {
	for _, test := range []struct {
		stage, reason string
		valid         bool
	}{
		{"body", "invalid_event", true},
		{"end", "partial_event", true},
		{"dispatch", "unrecognized_json", true},
		{"headers", "missing_terminal", true},
		{"body", "failed_event", true},
		{"end", "delivery_interrupted", true},
		{"private-stage", "invalid_event", false},
		{"body", "private-error", false},
	} {
		raw, err := json.Marshal(map[string]any{"msg": "plugin_business_unknown",
			"account_id": 42, "stage": test.stage, "reason": test.reason,
			"extra": "must-not-retain-private-data"})
		require.NoError(t, err)
		var output bytes.Buffer
		writer := &pluginEvidenceLog{logger: slog.New(slog.NewJSONHandler(&output, nil))}
		_, err = writer.Write(append(raw, '\n'))
		require.NoError(t, err)
		if test.valid {
			require.Contains(t, output.String(), test.reason)
			require.NotContains(t, output.String(), "must-not-retain")
		} else {
			require.Empty(t, output.String())
		}
	}
}
