package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/zeromicro/go-zero/core/collection"
)

var newTimingWheel = collection.NewTimingWheel

// TimingWheelService wraps go-zero's TimingWheel for task scheduling
type TimingWheelService struct {
	tw       *collection.TimingWheel
	stopOnce sync.Once
	mu       sync.Mutex
	stopped  bool
	wg       sync.WaitGroup
}

// NewTimingWheelService creates a new TimingWheelService instance
func NewTimingWheelService() (*TimingWheelService, error) {
	// 1 second tick, 3600 slots = supports up to 1 hour delay
	// execute function: runs func() type tasks
	s := &TimingWheelService{}
	tw, err := newTimingWheel(1*time.Second, 3600, func(key, value any) {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		s.wg.Add(1)
		s.mu.Unlock()
		defer s.wg.Done()
		if fn, ok := value.(func()); ok {
			fn()
		}
	})
	if err != nil {
		return nil, fmt.Errorf("创建 timing wheel 失败: %w", err)
	}
	s.tw = tw
	return s, nil
}

// Start starts the timing wheel
func (s *TimingWheelService) Start() {
	logger.LegacyPrintf("service.timing_wheel", "%s", "[TimingWheel] Started (auto-start by go-zero)")
}

// Stop stops the timing wheel
func (s *TimingWheelService) Stop() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		s.tw.Stop()
		logger.LegacyPrintf("service.timing_wheel", "%s", "[TimingWheel] Stopped")
	})
	s.wg.Wait()
}

// Schedule schedules a one-time task
func (s *TimingWheelService) Schedule(name string, delay time.Duration, fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if err := s.tw.SetTimer(name, fn, delay); err != nil {
		logger.LegacyPrintf("service.timing_wheel", "[TimingWheel] SetTimer failed for %q: %v", name, err)
	}
}

// ScheduleRecurring schedules a recurring task
func (s *TimingWheelService) ScheduleRecurring(name string, interval time.Duration, fn func()) {
	var schedule func()
	schedule = func() {
		fn()
		s.Schedule(name, interval, schedule)
	}
	s.Schedule(name, interval, schedule)
}

// Cancel cancels a scheduled task
func (s *TimingWheelService) Cancel(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	_ = s.tw.RemoveTimer(name)
}
