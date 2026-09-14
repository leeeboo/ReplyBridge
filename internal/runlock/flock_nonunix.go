//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package runlock

import "os"

// This file covers non-Unix platforms and Unix targets where x/sys/unix does
// not expose flock. Those targets must fail closed rather than continue
// without a process lock.
func flockSupported() error { return ErrUnsupported }

func acquireFileLock(*os.File) error { return ErrUnsupported }

func releaseFileLock(*os.File) error { return ErrUnsupported }
