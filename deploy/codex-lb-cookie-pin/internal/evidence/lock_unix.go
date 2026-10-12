//go:build darwin || linux

package evidence

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func privateMode(mode os.FileMode) bool { return mode.Perm()&0077 == 0 }

func lockFile(f *os.File) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil || (!errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN)) {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("evidence lock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func unlockFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
