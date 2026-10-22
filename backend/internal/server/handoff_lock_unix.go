//go:build !windows

package server

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func privateLock(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("absolute deployment lock path required")
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0007 != 0 {
		f.Close()
		return nil, errors.New("unsafe deployment lock")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		f.Close()
		return nil, errors.New("deployment lock replaced")
	}
	return f, nil
}
