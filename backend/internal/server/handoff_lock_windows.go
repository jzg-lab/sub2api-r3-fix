package server

import (
	"errors"
	"os"
)

func privateLock(string) (*os.File, error) {
	return nil, errors.New("deployment handoff is not supported on Windows")
}
