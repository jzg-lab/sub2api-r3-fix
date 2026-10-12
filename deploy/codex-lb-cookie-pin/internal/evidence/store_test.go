package evidence

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func diskStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	next, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	return next
}

var sampleCohort = Cohort{Model: "fixture-model", EffortForm: "omitted", Bucket: "direct"}

func TestBackoffLadderReloadAndSuccessDecay(t *testing.T) {
	s := diskStore(t)
	now := time.Now()
	for i, seconds := range []int{2, 4, 8, 16, 30, 30} {
		op, err := s.Begin(42, sampleCohort, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Dispatch(42, op, sampleCohort, now); err != nil {
			t.Fatal(err)
		}
		if err = s.Finish(42, op, sampleCohort, "failed", now, time.Time{}); err != nil {
			t.Fatal(err)
		}
		s = reopen(t, s)
		state, err := s.Read(42)
		if err != nil || state.Step != min(i+1, 5) || state.NotBefore.Sub(now) != time.Duration(seconds)*time.Second {
			t.Fatalf("wrong ladder: %+v %v", state, err)
		}
		if _, err := s.Begin(42, Cohort{Model: "replacement", EffortForm: "explicit-medium"}, now); !errors.Is(err, ErrBackoff) {
			t.Fatal("cohort/owner change reset backoff", err)
		}
		if err := s.Business(42, sampleCohort, "succeeded", now); err != nil {
			t.Fatal(err)
		}
		protected, _ := s.Read(42)
		if protected.Step != state.Step {
			t.Fatal("early success reduced a live backoff")
		}
		now = state.NotBefore
	}
	state, _ := s.Read(42)
	if state.Step != 5 {
		t.Fatal("time alone reduced backoff")
	}
	if err := s.Business(42, sampleCohort, "succeeded", now); err != nil {
		t.Fatal(err)
	}
	state, _ = s.Read(42)
	if state.Step != 4 {
		t.Fatal("elapsed real success did not reduce one step")
	}
}

func TestLongerDeadlinesAndCountsSurviveReload(t *testing.T) {
	s := diskStore(t)
	now := time.Now()
	op, _ := s.Begin(42, sampleCohort, now)
	if err := s.Finish(42, op, sampleCohort, "failed", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Policy(42, 3, now.Add(2*time.Hour), now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	s = reopen(t, s)
	state, err := s.Read(42)
	if err != nil || state.Deadline() != now.Add(2*time.Hour) || state.NotBefore != now.Add(time.Hour) || state.ConsecutiveFails != 3 {
		// JSON timestamps lose monotonic components, so compare instants below.
		if err != nil || !state.Deadline().Equal(now.Add(2*time.Hour)) ||
			!state.NotBefore.Equal(now.Add(time.Hour)) || state.ConsecutiveFails != 3 {
			t.Fatalf("lost protection: %+v %v", state, err)
		}
	}
	now = now.Add(3 * time.Hour)
	op, err = s.Begin(42, sampleCohort, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Dispatch(42, op, sampleCohort, now); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(42, op, sampleCohort, "pass", now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_ = s.Business(42, sampleCohort, "dispatched", now)
	_ = s.Business(42, sampleCohort, "failed", now)
	state, _ = reopen(t, s).Read(42)
	if state.ProbePassed != 1 || state.BusinessDispatched != 1 || state.BusinessSucceeded != 0 {
		t.Fatalf("probe and business counters mixed: %+v", state)
	}
}

func TestCrashAndOverlappingGenerationNeverReplay(t *testing.T) {
	for _, phase := range []string{"reserved", "dispatched"} {
		t.Run(phase, func(t *testing.T) {
			s := diskStore(t)
			now := time.Now()
			op, _ := s.Begin(42, sampleCohort, now)
			if phase == "dispatched" {
				if err := s.Dispatch(42, op, sampleCohort, now); err != nil {
					t.Fatal(err)
				}
			}
			// An intervening business event must not change the unknown's cohort.
			_ = s.Business(42, Cohort{Model: "other"}, "dispatched", now)
			next := reopen(t, s)
			state, err := next.Read(42)
			if err != nil || state.Operation.Phase != "unknown" || state.LastEvent.Cohort != sampleCohort {
				t.Fatalf("crash was silently retried or misattributed: %+v %v", state, err)
			}
			if _, err = next.Begin(42, sampleCohort, now.Add(24*time.Hour)); !errors.Is(err, ErrBlocked) {
				t.Fatal("new ID bypassed unknown fence", err)
			}
			if err = s.Finish(42, op, sampleCohort, "pass", now, time.Time{}); !errors.Is(err, ErrBlocked) {
				t.Fatal("late old generation converted unknown to success", err)
			}
			if err = next.Reconcile(42, op+"-stale", now); !errors.Is(err, ErrBlocked) {
				t.Fatal("stale reconciliation accepted", err)
			}
			if err = next.Reconcile(42, op, now); err != nil {
				t.Fatal(err)
			}
			state, _ = next.Read(42)
			if state.Operation.Phase != "unknown" || state.ProbePassed != 0 || state.Step != 1 {
				t.Fatal("reconciliation invented success or reset protection")
			}
			newID, err := next.Begin(42, sampleCohort, state.Deadline())
			if err != nil || newID == op {
				t.Fatal("reconciliation did not allow a distinct future operation", err)
			}
		})
	}
}

func TestJournalCorruptionAndPartialTail(t *testing.T) {
	for _, mode := range []string{"partial", "committed-corrupt", "first-partial", "wrong-account", "invalid-phase"} {
		t.Run(mode, func(t *testing.T) {
			s := diskStore(t)
			now := time.Now()
			op, _ := s.Begin(42, sampleCohort, now)
			_ = s.Finish(42, op, sampleCohort, "unknown", now, time.Time{})
			name := filepath.Join(s.dir, "42.jsonl")
			raw, _ := os.ReadFile(name)
			switch mode {
			case "partial":
				raw = append(raw, []byte(`{"version":1,"state":`)...)
			case "committed-corrupt":
				raw = append(raw, []byte("{}\n")...)
			case "first-partial":
				raw = []byte(`{"version":1`)
			case "wrong-account":
				raw = bytes.ReplaceAll(raw, []byte(`"account_id":42`), []byte(`"account_id":99`))
			case "invalid-phase":
				raw = bytes.ReplaceAll(raw, []byte(`"phase":"unknown"`), []byte(`"phase":"unexpected"`))
			}
			if err := os.WriteFile(name, raw, 0600); err != nil {
				t.Fatal(err)
			}
			next := reopen(t, s)
			st, err := next.Read(42)
			if mode != "partial" {
				if err == nil {
					t.Fatal("corrupt committed evidence was accepted")
				}
				return
			}
			if err != nil || st.Operation.Phase != "unknown" {
				t.Fatal("partial tail lost committed unknown", err)
			}
			if err = next.Reconcile(42, op, now); err != nil {
				t.Fatal(err)
			}
			repaired, _ := os.ReadFile(name)
			if bytes.Contains(repaired, []byte(`"state":{"version"`)) || repaired[len(repaired)-1] != '\n' {
				t.Fatal("append did not repair partial tail")
			}
		})
	}
}

func TestConcurrentStoresAndAlertDeduplication(t *testing.T) {
	s := diskStore(t)
	other := reopen(t, s)
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if err := store.Business(42, sampleCohort, "dispatched", time.Now()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	st, err := s.Read(42)
	if err != nil || st.BusinessDispatched != 40 {
		t.Fatalf("concurrent updates lost: %+v %v", st, err)
	}
	now := time.Now()
	first, err := s.Signal(42, sampleCohort, "turn_state_ticket", now)
	second, err2 := other.Signal(42, sampleCohort, "turn_state_ticket", now.Add(time.Second))
	nextWindow, err3 := other.Signal(42, sampleCohort, "turn_state_ticket", now.Add(10*time.Minute))
	if err != nil || err2 != nil || err3 != nil || !first || second || !nextWindow {
		t.Fatalf("bad durable deduplication: %v %v %v", first, second, nextWindow)
	}
}

func TestRotationAndInterruptedRotationRetainProtection(t *testing.T) {
	s := diskStore(t)
	now := time.Now()
	op, _ := s.Begin(42, sampleCohort, now)
	_ = s.Finish(42, op, sampleCohort, "unknown", now, now.Add(time.Hour))
	name := filepath.Join(s.dir, "42.jsonl")
	raw, _ := os.ReadFile(name)
	padding := bytes.Repeat([]byte{'\n'}, segmentBytes)
	if err := os.WriteFile(name, append(padding, raw...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Business(42, sampleCohort, "dispatched", now); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(name)
	if info.Size() > maxRecord {
		t.Fatal("journal failed to rotate")
	}
	// Crash after rename, before writing the first new active checkpoint.
	_ = os.Remove(name + ".previous")
	if err := os.Rename(name, name+".previous"); err != nil {
		t.Fatal(err)
	}
	other := reopen(t, s)
	states, err := other.Snapshot()
	if err != nil || len(states) != 1 || states[0].Operation.Phase != "unknown" ||
		states[0].BusinessDispatched != 1 || !states[0].NotBefore.Equal(now.Add(time.Hour)) {
		t.Fatalf("interrupted rotation lost checkpoint: %+v %v", states, err)
	}
	if _, err = other.Begin(42, sampleCohort, now.Add(2*time.Hour)); !errors.Is(err, ErrBlocked) {
		t.Fatal("rotation cleared unknown")
	}
}

func TestPersistenceFailsClosedWithoutReset(t *testing.T) {
	s := diskStore(t)
	// A directory at the account's journal path cannot be silently replaced.
	if err := os.Mkdir(filepath.Join(s.dir, "42.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin(42, sampleCohort, time.Now()); err == nil {
		t.Fatal("unsafe journal became an outbound permit")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.dir, 0755); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(s.dir); err == nil {
			t.Fatal("public state directory accepted")
		}
	}
}

func TestDispatchRechecksProtection(t *testing.T) {
	s := diskStore(t)
	now := time.Now()
	op, _ := s.Begin(42, sampleCohort, now)
	if err := s.Policy(42, 0, time.Time{}, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(42, op, sampleCohort, now); !errors.Is(err, ErrBlocked) {
		t.Fatal("late cooldown was bypassed")
	}
}
