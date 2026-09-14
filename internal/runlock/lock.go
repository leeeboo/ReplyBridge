// Package runlock provides a non-blocking, process-wide lock for a SQLite
// database's synchronization writer.
package runlock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	// ErrLocked is returned when another synchronization writer already holds
	// the lock for the same database.
	ErrLocked = errors.New("database synchronization is already locked")

	// ErrMultipleLinks is returned when an existing database has more than one
	// hard link. Such aliases would otherwise receive different sibling locks.
	ErrMultipleLinks = errors.New("database has multiple hard links")

	// ErrInvalidPath is returned when dbPath cannot identify a filesystem-backed
	// database with a sibling lock file.
	ErrInvalidPath = errors.New("invalid database path for synchronization lock")

	// ErrUnsupported is returned on platforms without the required advisory
	// file-lock implementation. Callers must not continue without a lock.
	ErrUnsupported = errors.New("database synchronization lock is unsupported on this platform")
)

var inProcessLocks = struct {
	sync.Mutex
	held map[string]struct{}
}{held: make(map[string]struct{})}

// Lock holds one database synchronization lock. Close releases the advisory
// lock but deliberately leaves its lock file in place for future acquisitions.
type Lock struct {
	file      *os.File
	path      string
	closeOnce sync.Once
	closeErr  error
}

// Acquire obtains the non-blocking exclusive synchronization lock associated
// with dbPath. dbPath must refer to a filesystem-backed database; in-memory
// SQLite databases have no stable sibling lock path.
func Acquire(dbPath string) (*Lock, error) {
	databasePath, err := normalizeDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}
	if err := flockSupported(); err != nil {
		return nil, err
	}
	if err := verifyDatabaseIdentity(databasePath); err != nil {
		return nil, err
	}

	lockPath := databasePath + ".sync.lock"
	if !reserveInProcess(lockPath) {
		return nil, ErrLocked
	}

	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseInProcess(lockPath)
		return nil, fmt.Errorf("open synchronization lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		releaseInProcess(lockPath)
		return nil, fmt.Errorf("set synchronization lock permissions: %w", err)
	}
	if err := acquireFileLock(file); err != nil {
		_ = file.Close()
		releaseInProcess(lockPath)
		return nil, err
	}

	return &Lock{file: file, path: lockPath}, nil
}

// Close releases the lock. It is safe to call more than once.
func (l *Lock) Close() error {
	if l == nil {
		return nil
	}

	l.closeOnce.Do(func() {
		unlockErr := releaseFileLock(l.file)
		closeErr := l.file.Close()
		releaseInProcess(l.path)
		l.closeErr = errors.Join(unlockErr, closeErr)
	})
	return l.closeErr
}

func normalizeDatabasePath(dbPath string) (string, error) {
	trimmed := strings.TrimSpace(dbPath)
	if trimmed == "" {
		return "", fmt.Errorf("%w: path is empty", ErrInvalidPath)
	}
	if trimmed == ":memory:" {
		return "", fmt.Errorf("%w: in-memory databases cannot be locked", ErrInvalidPath)
	}

	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("make database path absolute: %w", err)
	}
	absPath = filepath.Clean(absPath)

	if _, err := os.Lstat(absPath); err == nil {
		resolved, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", fmt.Errorf("resolve database path: %w", err)
		}
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect database path: %w", err)
	}

	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil {
		return "", fmt.Errorf("resolve database directory: %w", err)
	}
	return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(absPath)), nil
}

func reserveInProcess(path string) bool {
	inProcessLocks.Lock()
	defer inProcessLocks.Unlock()
	if _, held := inProcessLocks.held[path]; held {
		return false
	}
	inProcessLocks.held[path] = struct{}{}
	return true
}

func releaseInProcess(path string) {
	inProcessLocks.Lock()
	delete(inProcessLocks.held, path)
	inProcessLocks.Unlock()
}
