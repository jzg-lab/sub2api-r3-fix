package cookiestore

import (
	"testing"
	"time"
)

func TestCookieGenerationRejectsStaleCaptureAndReroll(t *testing.T) {
	s := New()
	now := time.Now()
	s.Capture(42, []string{"__cflb=old; Max-Age=3500"}, now)
	_, version := s.HeaderSnapshot(42, "", now)
	s.Capture(42, []string{"__cflb=new; Max-Age=3500"}, now)
	if _, ok := s.CaptureIfCurrent(42, []string{"__cflb=late; Max-Age=3500"}, now, version); ok {
		t.Fatal("accepted stale response")
	}
	if s.AcceptProbe(42, version, true) {
		t.Fatal("accepted stale reroll")
	}
	if s.MergeHeader(42, "", now) != "__cflb=new" {
		t.Fatal("new cookie was overwritten")
	}
	_, current := s.HeaderSnapshot(42, "", now)
	if !s.AcceptProbe(42, current, true) || s.MergeHeader(42, "", now) != "" {
		t.Fatal("current-generation reroll failed")
	}
	if s.AcceptProbe(42, current, false) {
		t.Fatal("drop did not invalidate old results")
	}
}

func TestDelayedRestoreDoesNotReviveDroppedCookies(t *testing.T) {
	s := New()
	now := time.Now()
	s.Capture(42, []string{"__cflb=old; Max-Age=3500"}, now)
	snapshot := s.SnapshotJSON()
	s.Drop(42)
	s.RestoreJSON(snapshot, now)
	if s.MergeHeader(42, "", now) != "" {
		t.Fatal("late restore revived a dropped cookie")
	}
}

func TestBusinessResponseRejectsStaleCaptureAndReroll(t *testing.T) {
	s := New()
	now := time.Now()
	s.Capture(42, []string{"__cflb=old; Max-Age=3500"}, now)
	_, old := s.HeaderSnapshot(42, "", now)
	s.Capture(42, []string{"__cflb=new; Max-Age=3500"}, now)
	if s.ObserveIfCurrent(42, []string{"__cflb=late; Max-Age=3500"}, now, old, true) {
		t.Fatal("accepted stale business response")
	}
	if s.MergeHeader(42, "", now) != "__cflb=new" || s.Status(now).Rerolls != 0 {
		t.Fatal("stale response changed the current cookie")
	}
	_, current := s.HeaderSnapshot(42, "", now)
	if !s.ObserveIfCurrent(42, []string{"__cflb=replaced; Max-Age=3500"}, now, current, true) {
		t.Fatal("current business response was rejected")
	}
	if s.MergeHeader(42, "", now) != "" || s.Status(now).Rerolls != 1 {
		t.Fatal("current reroll did not atomically discard the captured cookie")
	}
}
