package service

import (
	"context"
	"log"
	"sync"
	"time"
)

// DeferredService provides deferred batch update functionality
type DeferredService struct {
	accountRepo AccountRepository
	timingWheel *TimingWheelService
	interval    time.Duration

	lastUsedUpdates sync.Map
	mu              sync.Mutex
	flushMu         sync.Mutex
	inflight        int
}

// NewDeferredService creates a new DeferredService instance
func NewDeferredService(accountRepo AccountRepository, timingWheel *TimingWheelService, interval time.Duration) *DeferredService {
	return &DeferredService{
		accountRepo: accountRepo,
		timingWheel: timingWheel,
		interval:    interval,
	}
}

// Start starts the deferred service
func (s *DeferredService) Start() {
	s.timingWheel.ScheduleRecurring("deferred:last_used", s.interval, s.flushLastUsed)
	log.Printf("[DeferredService] Started (interval: %v)", s.interval)
}

// Stop stops the deferred service
func (s *DeferredService) Stop() {
	s.timingWheel.Cancel("deferred:last_used")
	s.flushLastUsed()
	log.Printf("[DeferredService] Service stopped")
}

func (s *DeferredService) ScheduleLastUsedUpdate(accountID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsedUpdates.Store(accountID, time.Now())
}

func (s *DeferredService) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := s.inflight
	s.lastUsedUpdates.Range(func(_, _ any) bool { pending++; return true })
	return pending
}

func (s *DeferredService) flushLastUsed() {
	_ = s.Flush(context.Background())
}

// Flush persists accepted updates immediately without stopping the recurring job.
func (s *DeferredService) Flush(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !s.flushMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	defer s.flushMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	updates := make(map[int64]time.Time)
	s.lastUsedUpdates.Range(func(key, value any) bool {
		id, ok := key.(int64)
		if !ok {
			return true
		}
		ts, ok := value.(time.Time)
		if !ok {
			return true
		}
		updates[id] = ts
		s.lastUsedUpdates.Delete(key)
		return true
	})
	s.inflight = len(updates)
	s.mu.Unlock()

	if len(updates) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := s.accountRepo.BatchUpdateLastUsed(ctx, updates)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = 0
	if err != nil {
		log.Printf("[DeferredService] BatchUpdateLastUsed failed (%d accounts): %v", len(updates), err)
		for id, ts := range updates {
			current, exists := s.lastUsedUpdates.Load(id)
			if !exists || ts.After(current.(time.Time)) {
				s.lastUsedUpdates.Store(id, ts)
			}
		}
	} else {
		log.Printf("[DeferredService] BatchUpdateLastUsed flushed %d accounts", len(updates))
	}
	return err
}
