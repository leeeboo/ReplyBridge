package bili

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReplaceCredentialRemovesStaleCookies(t *testing.T) {
	client := testClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client.SetCredential(Credential{Cookies: map[string]string{
		"SESSDATA": "old", "bili_jct": "old-csrf", "DedeUserID": "42", "legacy": "stale",
	}})
	client.ReplaceCredential(Credential{Cookies: map[string]string{
		"SESSDATA": "new", "bili_jct": "new-csrf", "DedeUserID": "42",
	}})
	credential := client.Credential()
	if credential.Cookies["SESSDATA"] != "new" || credential.Cookies["bili_jct"] != "new-csrf" || credential.Cookies["DedeUserID"] != "42" {
		t.Fatalf("replacement credential=%#v", credential.CookieNames())
	}
	if _, exists := credential.Cookies["legacy"]; exists {
		t.Fatalf("stale cookie survived replacement: %#v", credential.CookieNames())
	}
}

func TestLoadCredentialFileRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(target, []byte(`{"SESSDATA":"session","bili_jct":"csrf"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if credential, err := LoadCredentialFile(target); err != nil || credential.Cookies["SESSDATA"] != "session" {
		t.Fatalf("load private credential: %#v, %v", credential, err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentialFile(target); err == nil {
		t.Fatal("accepted group-readable credential")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "credential-link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCredentialFile(link); err == nil {
			t.Fatal("accepted symlink credential")
		}
	}
}
