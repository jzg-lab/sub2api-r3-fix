package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2PlazaKeepsUnlistedModelMetricsSeparate(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		name := "channel monitor"
		if preserve {
			name = "model plaza"
		}
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
			end := start.Add(time.Hour)
			filter := service.ChannelMonitorV2Filter{
				Start: start, End: end, Bucket: time.Hour,
				PreserveModelNames: preserve, IncludeOutputTPS: true,
				RestrictGroups: true, AllowedGroupIDs: []int64{37},
			}
			mock.ExpectQuery(`(?s)SELECT version, enabled.*FROM channel_monitor_v2_config`).WillReturnRows(
				sqlmock.NewRows([]string{"version", "enabled", "refresh", "platforms", "groups", "ignored", "thresholds", "updated_at", "updated_by"}).
					AddRow(1, true, 300, `[{"platform":"openai","enabled":true,"models":["gpt-5.6-sol"]}]`, "{37,45}", "{quota_or_balance}", "{}", start, nil))
			mock.ExpectQuery(`(?s)SELECT usage_coverage_start.*FROM channel_monitor_v2_watermarks`).WillReturnRows(
				sqlmock.NewRows([]string{"usage", "errors", "through", "computed", "backfill"}).AddRow(start, start, end, end, start))
			mock.ExpectQuery(`(?s)SELECT m.bucket_start.*FROM channel_monitor_v2_metrics_rollup`).
				WithArgs(start, end, pq.Array([]string{"openai"}), pq.Array([]int64{37}), 3600).WillReturnRows(
				sqlmock.NewRows([]string{"bucket", "platform", "group", "name", "model", "success", "errors", "affected", "attempts", "input", "output", "creation", "read", "ttft_sum", "ttft_count", "duration_sum", "duration_count"}).
					AddRow(start, "openai", 37, "GPT", "gpt-5.6-sol", 100, 0, 0, 0, 1000, 100, 0, 0, 20000, 100, 100000, 100).
					AddRow(start, "openai", 37, "GPT", "gpt-6.1-sol", 8, 2, 0, 0, 80, 80, 0, 0, 800, 8, 8000, 8).
					AddRow(start, "openai", 37, "GPT", "gpt-6.1-terra", 2, 0, 0, 0, 20, 20, 0, 0, 1000, 2, 2000, 2))
			mock.ExpectQuery(`(?s)SELECT h.bucket_start.*FROM channel_monitor_v2_latency_histograms_rollup`).
				WithArgs(start, end, pq.Array([]string{"openai"}), pq.Array([]int64{37}), 3600, 0).WillReturnRows(
				sqlmock.NewRows([]string{"bucket", "platform", "group", "model", "user", "metric", "bound", "count"}).
					AddRow(start, "openai", 37, "gpt-6.1-sol", 0, "ttft", 100, 8).
					AddRow(start, "openai", 37, "gpt-6.1-terra", 0, "ttft", 500, 2))
			mock.ExpectQuery(`SELECT id, COALESCE\(name, ''\).*FROM groups`).WithArgs(pq.Array([]int64{37})).WillReturnRows(
				sqlmock.NewRows([]string{"id", "name", "platform"}).AddRow(37, "GPT", "openai"))
			mock.ExpectQuery(`(?s)SELECT e.bucket_start.*FROM channel_monitor_v2_error_metrics_rollup`).
				WithArgs(start, end, pq.Array([]string{"openai"}), pq.Array([]int64{37}), 3600, pq.Array([]string{"quota_or_balance"}), service.ChannelMonitorV2TaxonomyVersion).WillReturnRows(
				sqlmock.NewRows([]string{"bucket", "platform", "group", "model", "errors"}).AddRow(start, "openai", 37, "gpt-6.1-sol", 1))
			mock.ExpectQuery(`(?s)SELECT m.platform, m.group_id, m.model, SUM\(m.output_tps\).*FROM usage_logs`).
				WithArgs(start, end, pq.Array([]string{"openai"}), pq.Array([]int64{37})).WillReturnRows(
				sqlmock.NewRows([]string{"platform", "group", "model", "sum", "count"}).
					AddRow("openai", 37, "gpt-5.6-sol", 500.0, 10).
					AddRow("openai", 37, "gpt-6.1-sol", 90.0, 3).
					AddRow("openai", 37, "gpt-6.1-terra", 90.0, 1))

			matrix, err := service.NewChannelMonitorV2Service(NewChannelMonitorV2Repository(db)).Matrix(context.Background(), filter, service.ChannelMonitorV2GroupByPlatformGroupModel, true)
			require.NoError(t, err)
			rows := map[string]service.ChannelMonitorV2MatrixRow{}
			for _, row := range matrix.Items {
				require.Equal(t, int64(37), *row.GroupID)
				rows[row.Model] = row
			}
			require.Equal(t, int64(100), rows["gpt-5.6-sol"].Metrics.RequestCount)
			require.InDelta(t, 50.0, *rows["gpt-5.6-sol"].OutputTPS, 0.001)
			if preserve {
				require.NotContains(t, rows, service.ChannelMonitorV2OtherModel)
				sol, terra := rows["gpt-6.1-sol"], rows["gpt-6.1-terra"]
				require.Equal(t, int64(10), sol.Metrics.RequestCount)
				require.InDelta(t, 0.8, sol.Metrics.SuccessRate, 0.001)
				require.InDelta(t, 0.1, sol.Metrics.ErrorRate, 0.001)
				require.Equal(t, int64(100), *sol.Metrics.TTFT.P50Ms)
				require.InDelta(t, 30.0, *sol.OutputTPS, 0.001)
				require.Len(t, sol.Buckets, 1)
				require.Equal(t, "warning", sol.Buckets[0].Health.ErrorRate)
				require.Equal(t, int64(2), terra.Metrics.RequestCount)
				require.Equal(t, int64(500), *terra.Metrics.TTFT.P50Ms)
				require.InDelta(t, 90.0, *terra.OutputTPS, 0.001)
				require.Equal(t, "healthy", terra.Buckets[0].Health.ErrorRate)
			} else {
				require.NotContains(t, rows, "gpt-6.1-sol")
				require.Equal(t, int64(12), rows[service.ChannelMonitorV2OtherModel].Metrics.RequestCount)
				require.InDelta(t, 45.0, *rows[service.ChannelMonitorV2OtherModel].OutputTPS, 0.001)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
