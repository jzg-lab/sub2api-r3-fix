package main

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCleanupOrdersProducersBeforeConsumersAndInfrastructure(t *testing.T) {
	var mu sync.Mutex
	var completed []string
	step := func(name string) cleanupStep {
		return cleanupStep{name, func() error {
			mu.Lock()
			defer mu.Unlock()
			completed = append(completed, name)
			return nil
		}}
	}
	require.NoError(t, runCleanup([]cleanupStep{
		step("AuditLogService"), step("PluginManager"), step("DeferredService"),
		step("RequestSideEffects"), step("SubscriptionService"),
		step("UsageRecordWorkerPool"), step("TimingWheelService"),
		step("RequestSideEffectsBeforeQueues"),
	}, []cleanupStep{step("Redis"), step("Ent")}))
	require.Equal(t, []string{"TimingWheelService", "RequestSideEffectsBeforeQueues", "UsageRecordWorkerPool", "SubscriptionService",
		"RequestSideEffects", "DeferredService", "PluginManager", "AuditLogService", "Redis", "Ent"}, completed)
}

func TestCleanupFailureDoesNotCloseDownstreamDependencies(t *testing.T) {
	for _, panicStep := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panicStep], func(t *testing.T) {
			called := false
			downstream := cleanupStep{"UsageRecordWorkerPool", func() error { called = true; return nil }}
			err := runCleanup([]cleanupStep{{"Producer", func() error {
				if panicStep {
					panic("failed")
				}
				return errors.New("failed")
			}}, downstream}, []cleanupStep{{"Redis", downstream.fn}})
			require.ErrorContains(t, err, "Producer")
			require.False(t, called)
		})
	}
}
