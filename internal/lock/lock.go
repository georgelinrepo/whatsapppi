// Package lock stops two feedbridge modes running at once.
package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

var ErrBusy = errors.New("run in progress")

// Acquire takes an exclusive non-blocking flock on path (created 0600).
// release unlocks and closes the file; it does not delete it.
func Acquire(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock: flock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
