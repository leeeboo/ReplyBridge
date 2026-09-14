package secretfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPrivateFile(t *testing.T) {
	path := writePrivateFile(t, "token-value\n", 0o600)

	got, err := Load(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "token-value\n" {
		t.Fatalf("Load() = %q", got)
	}

	trimmed, err := LoadTrimmedString(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed != "token-value" {
		t.Fatalf("LoadTrimmedString() = %q", trimmed)
	}
}

func TestLoadRejectsUnsafeFiles(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		max  int
		want string
	}{
		{
			name: "empty path",
			path: func(t *testing.T) string { return "" },
			max:  16,
			want: "path is required",
		},
		{
			name: "invalid limit",
			path: func(t *testing.T) string { return writePrivateFile(t, "value", 0o600) },
			max:  0,
			want: "invalid size limit",
		},
		{
			name: "group readable",
			path: func(t *testing.T) string { return writePrivateFile(t, "value", 0o640) },
			max:  16,
			want: "permissions",
		},
		{
			name: "directory",
			path: func(t *testing.T) string { return t.TempDir() },
			max:  16,
			want: "regular",
		},
		{
			name: "empty file",
			path: func(t *testing.T) string { return writePrivateFile(t, "", 0o600) },
			max:  16,
			want: "empty",
		},
		{
			name: "over limit",
			path: func(t *testing.T) string { return writePrivateFile(t, "too-long", 0o600) },
			max:  3,
			want: "size limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path(t), tt.max)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want category %q", err, tt.want)
			}
		})
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := Load(link, 16)
	if err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("Load() error = %v, want symbolic-link rejection", err)
	}
}

func TestLoadErrorsDoNotExposeContents(t *testing.T) {
	path := writePrivateFile(t, "not-for-output", 0o644)
	_, err := Load(path, 64)
	if err == nil {
		t.Fatal("Load() succeeded")
	}
	if strings.Contains(err.Error(), "not-for-output") {
		t.Fatalf("Load() leaked file contents in %q", err)
	}
}

func writePrivateFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}
