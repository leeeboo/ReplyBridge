//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runlock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// verifyDatabaseIdentity rejects existing hard-linked databases. A sibling
// lock file is path-based, so hard-link aliases would otherwise lock different
// files while writing to the same SQLite inode.
func verifyDatabaseIdentity(databasePath string) error {
	info, err := os.Stat(databasePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect database identity: %w", err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect database hard-link count: unexpected stat type %T", info.Sys())
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("%w: database link count is %d; refusing unsafe sibling lock", ErrMultipleLinks, stat.Nlink)
	}
	return nil
}
