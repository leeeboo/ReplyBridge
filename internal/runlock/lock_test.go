package runlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireRejectsEmptyAndMemoryPaths(t *testing.T) {
	for _, path := range []string{"", "   ", ":memory:", " :memory: "} {
		t.Run(path, func(t *testing.T) {
			lock, err := Acquire(path)
			if lock != nil {
				t.Fatal("Acquire returned a lock for an invalid path")
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("Acquire(%q) error = %v, want ErrInvalidPath", path, err)
			}
		})
	}
}

func TestAcquireIsExclusiveUntilClose(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bilibili.db")
	first := acquireForTest(t, databasePath)

	second, err := Acquire(databasePath)
	if second != nil {
		_ = second.Close()
		t.Fatal("second Acquire unexpectedly succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire error = %v, want ErrLocked", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	third := acquireForTest(t, databasePath)
	if err := third.Close(); err != nil {
		t.Fatalf("third Close() error = %v", err)
	}
}

func TestAcquireUsesCanonicalPathForSymlinkAliases(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "database.db")
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatalf("create database fixture: %v", err)
	}
	aliasPath := filepath.Join(dir, "database-alias.db")
	if err := os.Symlink(databasePath, aliasPath); err != nil {
		t.Skipf("create symbolic-link fixture: %v", err)
	}

	first := acquireForTest(t, databasePath)
	defer first.Close()

	second, err := Acquire(aliasPath)
	if second != nil {
		_ = second.Close()
		t.Fatal("Acquire through symbolic-link alias unexpectedly succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire through symbolic-link alias error = %v, want ErrLocked", err)
	}
}

func TestAcquireResolvesParentSymlinkForNewDatabase(t *testing.T) {
	dir := t.TempDir()
	realParent := filepath.Join(dir, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatalf("create real parent: %v", err)
	}
	aliasParent := filepath.Join(dir, "alias")
	if err := os.Symlink(realParent, aliasParent); err != nil {
		t.Skipf("create symbolic-link fixture: %v", err)
	}

	aliasPath := filepath.Join(aliasParent, "new-database.db")
	canonicalPath := filepath.Join(realParent, "new-database.db")
	first := acquireForTest(t, aliasPath)
	defer first.Close()

	second, err := Acquire(canonicalPath)
	if second != nil {
		_ = second.Close()
		t.Fatal("Acquire through canonical parent unexpectedly succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire through canonical parent error = %v, want ErrLocked", err)
	}
}

func TestAcquireUsesCanonicalPathForRelativePath(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "database.db")
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relativePath, err := filepath.Rel(workingDirectory, databasePath)
	if err != nil {
		t.Fatalf("make relative path: %v", err)
	}

	first := acquireForTest(t, relativePath)
	defer first.Close()

	second, err := Acquire(databasePath)
	if second != nil {
		_ = second.Close()
		t.Fatal("Acquire through absolute path unexpectedly succeeded")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("Acquire through absolute path error = %v, want ErrLocked", err)
	}
}

func TestAcquireRejectsHardLinkedDatabase(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "database.db")
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatalf("create database fixture: %v", err)
	}
	aliasPath := filepath.Join(dir, "database-alias.db")
	if err := os.Link(databasePath, aliasPath); err != nil {
		t.Skipf("create hard-link fixture: %v", err)
	}

	for _, path := range []string{databasePath, aliasPath} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			lock, err := Acquire(path)
			if errors.Is(err, ErrUnsupported) {
				t.Skip("advisory file locks are unsupported on this platform")
			}
			if lock != nil {
				_ = lock.Close()
				t.Fatal("Acquire unexpectedly succeeded for a hard-linked database")
			}
			if !errors.Is(err, ErrMultipleLinks) {
				t.Fatalf("Acquire(%q) error = %v, want ErrMultipleLinks", path, err)
			}
		})
	}

	if err := os.Remove(aliasPath); err != nil {
		t.Fatalf("remove hard-link alias: %v", err)
	}
	lock := acquireForTest(t, databasePath)
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() after removing hard-link alias error = %v", err)
	}
}

func TestAcquireRejectsAliasCreatedAfterLock(t *testing.T) {
	dir := t.TempDir()
	databasePath := filepath.Join(dir, "database.db")
	if err := os.WriteFile(databasePath, nil, 0o600); err != nil {
		t.Fatalf("create database fixture: %v", err)
	}

	first := acquireForTest(t, databasePath)
	defer first.Close()

	aliasPath := filepath.Join(dir, "database-alias.db")
	if err := os.Link(databasePath, aliasPath); err != nil {
		t.Skipf("create hard-link fixture: %v", err)
	}
	second, err := Acquire(aliasPath)
	if second != nil {
		_ = second.Close()
		t.Fatal("Acquire unexpectedly succeeded for a new hard-link alias")
	}
	if !errors.Is(err, ErrMultipleLinks) {
		t.Fatalf("Acquire(%q) error = %v, want ErrMultipleLinks", aliasPath, err)
	}
}

func TestLockFilePersistsWithOwnerOnlyPermissions(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "database.db")
	lock := acquireForTest(t, databasePath)
	if err := lock.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	info, err := os.Stat(databasePath + ".sync.lock")
	if err != nil {
		t.Fatalf("stat persistent lock file: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("lock file mode = %04o, want %04o", got, want)
	}
}

func acquireForTest(t *testing.T, path string) *Lock {
	t.Helper()
	lock, err := Acquire(path)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("advisory file locks are unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Acquire(%q) error = %v", path, err)
	}
	return lock
}
