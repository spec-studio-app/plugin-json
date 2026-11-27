//go:build unix

package files

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile takes an exclusive flock on path and returns the function that
// lets it go. A flock belongs to one open file, so it keeps two goroutines
// apart and two processes apart. The lock goes when the process ends, also at
// a crash.
func LockFile(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
