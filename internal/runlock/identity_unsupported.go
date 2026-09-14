//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris)

package runlock

// Acquire checks flockSupported before calling this function. Keep this
// explicit for unsupported targets so future call-order changes cannot create
// a silent no-lock path.
func verifyDatabaseIdentity(string) error { return ErrUnsupported }
