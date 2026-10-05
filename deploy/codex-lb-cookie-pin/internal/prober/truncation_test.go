package prober

import (
	"testing"
	"time"
)

func TestTruncationWarningDoesNotPenalizeQuality(t *testing.T) {
	cfg := testCfg()
	st := NewState(9)
	st.ConsecFails, st.ConsecPasses = 1, 2
	now := time.Now()
	for i := range 5 {
		decision := st.Record(VerdictError, "q", "trunc-fp:1034", now.Add(time.Duration(i)*time.Minute), cfg)
		if decision != (Decision{}) || st.Fails != 0 || st.QualityRerolls != 0 ||
			st.ConsecFails != 1 || st.ConsecPasses != 0 || st.SuspectAccountLevel {
			t.Fatalf("inconclusive fingerprint changed quality state: %+v / %+v", st, decision)
		}
		if st.TruncationRateAlert != (i == 4) {
			t.Fatalf("unexpected alert at sample %d", i+1)
		}
	}
	for i := range 20 {
		st.Record(VerdictPass, "q", "correct", now.Add(time.Duration(i+5)*time.Minute), cfg)
	}
	if st.TruncationRateAlert || st.TruncationWindowSamples != 20 || st.TruncationWindowHits != 0 ||
		st.TruncationObservations != 5 {
		t.Fatalf("rolling warning did not expire: %+v", st)
	}
}

func TestBackoffRetainsFailureEvidenceUntilNextCycle(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	st := NewState(9)
	for i := 0; i < cfg.MaxConsecutiveProbeFailures; i++ {
		st.Record(VerdictFail, "q", "wrong", now, cfg)
	}
	if st.ConsecFails != cfg.MaxConsecutiveProbeFailures || !st.SuspectAccountLevel || st.Due(now) {
		t.Fatalf("backoff lost its failure evidence: %+v", st)
	}
	until := st.BackoffUntil
	decision := st.Record(VerdictFail, "q", "wrong", until, cfg)
	if !decision.ShouldReroll || decision.EnterBackoff || st.ConsecFails != 1 {
		t.Fatalf("post-backoff attempt did not receive a fresh failure budget: %+v / %+v", st, decision)
	}
}

func TestPassStreakIsBoundToCurrentCleanEpisode(t *testing.T) {
	cfg := testCfg()
	now := time.Now()
	for _, interrupt := range []Verdict{VerdictFail, VerdictError} {
		st := NewState(9)
		st.Record(VerdictPass, "q", "correct", now, cfg)
		st.Record(VerdictPass, "q", "correct", now.Add(time.Minute), cfg)
		if !st.PreviousPassAt.Equal(now) || st.ConsecPasses != 2 {
			t.Fatalf("consecutive passes lost their previous observation: %+v", st)
		}
		st.Record(VerdictPass, "q", "correct", now.Add(90*time.Second), cfg)
		if !st.PreviousPassAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("previous pass must advance while a healthy streak continues: %+v", st)
		}
		st.Record(interrupt, "q", "interrupted", now.Add(2*time.Minute), cfg)
		if !st.PreviousPassAt.IsZero() {
			t.Fatalf("interrupted streak retained recovery evidence: %+v", st)
		}
		restarted := now.Add(3 * time.Minute)
		st.Record(VerdictPass, "q", "correct", restarted, cfg)
		if !st.PreviousPassAt.IsZero() || st.ConsecPasses != 1 {
			t.Fatalf("new streak reused the previous episode: %+v", st)
		}
	}
}
