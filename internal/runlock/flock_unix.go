//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runlock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func flockSupported() error { return nil }

func acquireFileLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return fmt.Errorf("%w: %v", ErrLocked, err)
	}
	return fmt.Errorf("acquire synchronization lock: %w", err)
}

func releaseFileLock(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("release synchronization lock: %w", err)
	}
	return nil
}
