// Package envfile loads a small, non-executable subset of dotenv files.
package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// LoadIfExists loads path without overriding variables already present in the
// process environment. Missing files are ignored. Values are never included in
// returned errors.
func LoadIfExists(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open env file: %w", err)
	}
	defer f.Close()
	return Load(f, os.LookupEnv, os.Setenv)
}

// Load parses KEY=VALUE lines. It deliberately does not perform shell
// expansion, command substitution, or variable interpolation.
func Load(r io.Reader, lookup func(string) (string, bool), set func(string, string) error) error {
	if r == nil || lookup == nil || set == nil {
		return errors.New("env file loader is not configured")
	}
	scanner := bufio.NewScanner(r)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !validKey(key) {
			return fmt.Errorf("invalid env file line %d", lineNo)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if first, last := value[0], value[len(value)-1]; (first == '\'' && last == '\'') || (first == '"' && last == '"') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := lookup(key); exists {
			continue
		}
		if err := set(key, value); err != nil {
			return fmt.Errorf("set env file key on line %d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read env file: %w", err)
	}
	return nil
}

func validKey(key string) bool {
	for i, r := range key {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return key != ""
}
