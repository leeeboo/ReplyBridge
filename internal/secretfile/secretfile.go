// Package secretfile loads secrets from private, regular files.
//
// It deliberately has no write operations. Callers should pass the resulting
// bytes directly to the API that needs them and must not log their contents.
package secretfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Load reads a non-empty secret from path. The file must be regular, must not
// be a symlink, and must not be accessible by group or other users. maxBytes
// is a strict upper bound on the returned data.
//
// Errors identify only the path and a safe failure category; they never
// include file contents or underlying operating-system error text.
func Load(path string, maxBytes int) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, loadError(path, "path is required")
	}
	if maxBytes <= 0 {
		return nil, loadError(path, "invalid size limit")
	}

	before, err := os.Lstat(path)
	if err != nil {
		return nil, loadError(path, "cannot stat file")
	}
	if err := validateInfo(before, maxBytes); err != nil {
		return nil, loadError(path, err.Error())
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, loadError(path, "cannot open file")
	}
	defer f.Close()

	after, err := f.Stat()
	if err != nil {
		return nil, loadError(path, "cannot stat opened file")
	}
	// Lstat before opening rejects symlinks. SameFile verifies the descriptor
	// still names that exact filesystem object after open, closing the normal
	// path-replacement window between Lstat and Open.
	if !os.SameFile(before, after) {
		return nil, loadError(path, "file changed while opening")
	}
	if err := validateInfo(after, maxBytes); err != nil {
		return nil, loadError(path, err.Error())
	}

	readLimit := int64(maxBytes)
	// maxBytes can be MaxInt on 64-bit platforms. There is no representable
	// byte count larger than MaxInt64, so the pre/post Stat checks are already
	// sufficient in that edge case; avoid overflowing the reader limit.
	if readLimit < int64(^uint64(0)>>1) {
		readLimit++
	}
	data, err := io.ReadAll(io.LimitReader(f, readLimit))
	if err != nil {
		return nil, loadError(path, "cannot read file")
	}
	if len(data) == 0 {
		return nil, loadError(path, "file is empty")
	}
	if len(data) > maxBytes {
		return nil, loadError(path, "file exceeds size limit")
	}
	return data, nil
}

// LoadTrimmedString loads a secret and returns its surrounding whitespace
// removed. It rejects a file whose resulting value is empty.
func LoadTrimmedString(path string, maxBytes int) (string, error) {
	data, err := Load(path, maxBytes)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", loadError(path, "file is empty")
	}
	return value, nil
}

func validateInfo(info os.FileInfo, maxBytes int) error {
	mode := info.Mode()
	if mode&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not allowed")
	}
	if !mode.IsRegular() {
		return errors.New("file must be regular")
	}
	if mode.Perm()&0o077 != 0 {
		return errors.New("file permissions must exclude group and other users")
	}
	if info.Size() == 0 {
		return errors.New("file is empty")
	}
	if info.Size() > int64(maxBytes) {
		return errors.New("file exceeds size limit")
	}
	return nil
}

func loadError(path, category string) error {
	return fmt.Errorf("secret file %q: %s", path, category)
}
