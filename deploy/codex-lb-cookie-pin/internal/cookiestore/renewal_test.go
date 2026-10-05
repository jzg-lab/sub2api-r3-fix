package cookiestore

import (
	"testing"
	"time"
)

func TestSameLiveCookieRenewalPreservesSignatureAge(t *testing.T) {
	store := New()
	now := time.Now()
	store.Capture(42, []string{"__cflb=fixture; Max-Age=100"}, now)
	_, version := store.HeaderSnapshot(42, "", now)
	refreshed := now.Add(50 * time.Second)
	nextVersion, current := store.CaptureIfCurrent(42, []string{"__cflb=fixture; Max-Age=300"}, refreshed, version)
	if !current || nextVersion == version {
		t.Fatal("renewal must still fence stale in-flight responses")
	}
	info := store.SignInfo(42, refreshed)
	if !info.CapturedAt.Equal(now) || info.Stats.Samples != 0 {
		t.Fatal("same live cookie was counted as a replacement signature")
	}
	if got := store.MergeHeader(42, "", now.Add(200*time.Second)); got != "__cflb=fixture" {
		t.Fatal("renewed expiry was not applied")
	}
	if store.AcceptProbe(42, version, true) {
		t.Fatal("old probe must not reroll a renewed cookie")
	}
	store.Capture(42, []string{"__cflb=replacement; Max-Age=300"}, now.Add(210*time.Second))
	info = store.SignInfo(42, now.Add(210*time.Second))
	if !info.CapturedAt.Equal(now.Add(210*time.Second)) || info.Stats.Samples != 1 || info.Stats.P50 != 210 {
		t.Fatal("real replacement did not retain the full signature lifetime")
	}
}

func TestExpiredSameValueIsANewSignature(t *testing.T) {
	for _, gap := range []time.Duration{time.Minute, 2 * time.Minute} {
		store := New()
		now := time.Now()
		store.Capture(42, []string{"__cflb=fixture; Max-Age=60"}, now)
		store.Capture(42, []string{"__cflb=fixture; Max-Age=300"}, now.Add(gap))
		info := store.SignInfo(42, now.Add(gap))
		if !info.CapturedAt.Equal(now.Add(gap)) || info.Stats.Samples != 1 {
			t.Fatalf("expired signature incorrectly retained its original age at %s", gap)
		}
	}
}
