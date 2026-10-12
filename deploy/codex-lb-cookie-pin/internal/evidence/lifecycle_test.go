package evidence

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSuccessRequiresDispatchedMatchingOperation(t *testing.T) {
	s := diskStore(t)
	now := time.Now()
	op, err := s.Begin(42, sampleCohort, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range []string{"pass", "observed"} {
		if err := s.Finish(42, op, sampleCohort, result, now, time.Time{}); !errors.Is(err, ErrBlocked) {
			t.Fatalf("reserved operation accepted %s: %v", result, err)
		}
	}
	wrong := sampleCohort
	wrong.EffortForm = "explicit-medium"
	if err := s.Dispatch(42, op, wrong, now); !errors.Is(err, ErrBlocked) {
		t.Fatal("dispatch accepted another cohort")
	}
	if err := s.Dispatch(42, op, sampleCohort, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(42, op, wrong, "pass", now, time.Time{}); !errors.Is(err, ErrBlocked) {
		t.Fatal("finish accepted another cohort")
	}
	if err := s.Finish(42, op, sampleCohort, "pass", now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(42, op, sampleCohort, "pass", now, time.Time{}); !errors.Is(err, ErrBlocked) {
		t.Fatal("terminal operation was counted twice")
	}
	st, err := s.Read(42)
	if err != nil || st.ProbePassed != 1 || st.BusinessSucceeded != 0 {
		t.Fatalf("incorrect counters: %+v %v", st, err)
	}
}

func TestEvidenceCrashChild(t *testing.T) {
	dir := os.Getenv("SUB2API_EVIDENCE_CRASH_FIXTURE")
	if dir == "" {
		return
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.Begin(42, sampleCohort, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(42, op, sampleCohort, time.Now()); err != nil {
		t.Fatal(err)
	}
	fmt.Println("dispatched")
	time.Sleep(time.Minute)
	t.Fatal("parent did not interrupt fixture process")
}

func TestKilledProcessRetainsUnknownFence(t *testing.T) {
	s := diskStore(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEvidenceCrashChild$")
	cmd.Env = append(os.Environ(), "SUB2API_EVIDENCE_CRASH_FIXTURE="+s.dir)
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		ready <- scanner.Scan() && scanner.Text() == "dispatched"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child did not commit dispatch")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child did not reach crash boundary")
	}
	_ = cmd.Process.Kill()
	if err := cmd.Wait(); err == nil {
		t.Fatal("fixture was not interrupted")
	}
	recovered := reopen(t, s)
	state, err := recovered.Read(42)
	if err != nil || state.Operation.Phase != "unknown" {
		t.Fatalf("interrupted dispatch was not recovered: %+v %v", state, err)
	}
	if _, err := recovered.Begin(42, sampleCohort, time.Now().Add(time.Hour)); !errors.Is(err, ErrBlocked) {
		t.Fatal("process crash allowed replay", err)
	}
}

func TestEvidenceLockWaitIsBounded(t *testing.T) {
	s := diskStore(t)
	f, err := openPrivate(filepath.Join(s.dir, "42.jsonl.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		t.Fatal(err)
	}
	defer unlockFile(f)
	start := time.Now()
	if _, err := s.Begin(42, sampleCohort, start); err == nil {
		t.Fatal("competing writer bypassed lock")
	}
	if elapsed := time.Since(start); elapsed < time.Second || elapsed > 4*time.Second {
		t.Fatalf("unexpected lock deadline: %v", elapsed)
	}
}

func BenchmarkDurableBusinessEvent(b *testing.B) {
	dir := b.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		b.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Business(42, sampleCohort, "dispatched", time.Now()); err != nil {
			b.Fatal(err)
		}
	}
}
