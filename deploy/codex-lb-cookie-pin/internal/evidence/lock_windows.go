package evidence

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// Windows mode bits do not express ACL privacy. State inherits the ACL of the
// host-owned plugin root; regular-file and symlink checks still apply.
func privateMode(os.FileMode) bool { return true }

func lockFile(f *os.File) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
		if err == nil || !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("evidence lock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func syncDirectory(string) error { return nil }
