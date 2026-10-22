package server

import "testing"

func TestHandoffLockUnsupportedOnWindows(t *testing.T) {
	file, err := privateLock("unused")
	if file != nil || err == nil {
		t.Fatal("Windows handoff must fail closed without acquiring a lock")
	}
}
