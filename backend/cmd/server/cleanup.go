package main

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

type cleanupStep struct {
	name string
	fn   func() error
}

// Producers finish before their downstream queues; infrastructure closes last.
func cleanupStage(name string) int {
	switch name {
	case "RequestSideEffectsBeforeQueues":
		return 1
	case "UsageRecordWorkerPool":
		return 2
	case "SubscriptionService":
		return 3
	case "RequestSideEffects":
		return 4
	case "DeferredService", "UserPlatformQuotaUsageFlusher", "BillingCacheService", "EmailQueueService", "PromptAuditService":
		return 5
	case "OpenAIWSPool":
		return 5
	case "PluginManager":
		return 6
	case "AuditLogService", "OpsSystemLogSink", "OpsIngressRejectAggregator", "OpsErrorLogWorkers":
		return 7
	default:
		return 0
	}
}

func runCleanup(steps, infrastructure []cleanupStep) error {
	run := func(step cleanupStep) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("%s panicked: %v", step.name, recovered)
			}
		}()
		if err := step.fn(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		log.Printf("[Cleanup] %s succeeded", step.name)
		return nil
	}
	for stage := 0; stage <= 7; stage++ {
		var wg sync.WaitGroup
		failures := make(chan error, len(steps))
		for _, step := range steps {
			if cleanupStage(step.name) != stage {
				continue
			}
			wg.Add(1)
			go func() { defer wg.Done(); failures <- run(step) }()
		}
		wg.Wait()
		close(failures)
		var result error
		for err := range failures {
			result = errors.Join(result, err)
		}
		if result != nil {
			return result
		}
	}
	for _, step := range infrastructure {
		if err := run(step); err != nil {
			return err
		}
	}
	log.Printf("[Cleanup] All cleanup steps completed")
	return nil
}
