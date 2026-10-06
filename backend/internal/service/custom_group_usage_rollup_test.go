//go:build unit

package service

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func useGroupUsageTestTimezone(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv("SUB2API_TEST_TIMEZONE") == name {
		return true
	}
	// Initialize once in a child process; changing time.Local races with
	// background workers left alive by otherwise unrelated service tests.
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "SUB2API_TEST_TIMEZONE="+name)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	return false
}

func TestGroupUsageDateUsesConfiguredTimezoneBoundary(t *testing.T) {
	if !useGroupUsageTestTimezone(t, "America/New_York") {
		return
	}

	beforeMidnight := time.Date(2026, 3, 9, 3, 59, 59, 0, time.UTC)
	atMidnight := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)

	require.Equal(t, "2026-03-08", GroupUsageDate(beforeMidnight))
	require.Equal(t, "2026-03-09", GroupUsageDate(atMidnight))
	require.Equal(t, atMidnight, GroupUsageTodayStart(atMidnight))
}

func TestGroupUsageParseDateUsesConfiguredTimezone(t *testing.T) {
	if !useGroupUsageTestTimezone(t, "America/New_York") {
		return
	}

	parsed, err := ParseGroupUsageDate("2026-03-08")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), parsed.UTC())
	require.Equal(t, "America/New_York", parsed.Location().String())
}

func TestGroupUsageYesterdayStartHandlesDST(t *testing.T) {
	if !useGroupUsageTestTimezone(t, "America/New_York") {
		return
	}

	todayStart := time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC)
	yesterdayStart := GroupUsageYesterdayStart(todayStart)

	require.Equal(t, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), yesterdayStart)
	require.Equal(t, 23*time.Hour, todayStart.Sub(yesterdayStart))
	require.Equal(t, "America/New_York", GroupUsageTimezoneName())
}
