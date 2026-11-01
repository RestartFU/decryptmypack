package downloader

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/oauth2"
)

func TestWriteTokenFileIsPrivate(t *testing.T) {
	path := t.TempDir() + "/token.json"

	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("token file permissions = %v, want 0600", got)
	}
}

func TestWriteTokenFileAtomicallyReplacesExistingCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "first"}); err != nil {
		t.Fatal(err)
	}
	oldFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()

	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "second"}); err != nil {
		t.Fatal(err)
	}
	oldBody, err := io.ReadAll(oldFile)
	if err != nil {
		t.Fatal(err)
	}
	var oldToken oauth2.Token
	if err := json.Unmarshal(oldBody, &oldToken); err != nil {
		t.Fatalf("decode original file handle: %v", err)
	}
	if oldToken.AccessToken != "first" {
		t.Fatalf("original file handle saw access token %q, want first", oldToken.AccessToken)
	}
	newToken, ok := readTokenFile(path)
	if !ok || newToken.AccessToken != "second" {
		t.Fatalf("replacement cache = (%#v, %v), want second token", newToken, ok)
	}
}

func TestWriteTokenFileCreatesPrivateParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "minecraft", "token.json")
	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("token directory permissions = %v, want no group/world access", got)
	}
}

func TestWriteTokenFileTightensExistingParentDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "token.json")
	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("token directory permissions = %v, want 0700", got)
	}
}

func TestNewAdapterUsesConfiguredTokenPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	adapter := New(path)
	if adapter.tokenPath != path {
		t.Fatalf("tokenPath = %q, want %q", adapter.tokenPath, path)
	}
}

func TestNewWithTokenSourceUsesProvidedAccount(t *testing.T) {
	source := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "account-token"})
	adapter := NewWithTokenSource(source)
	if adapter.tokenSource != source {
		t.Fatal("adapter did not retain the provided account token source")
	}
	if adapter.tokenPath != "" {
		t.Fatalf("tokenPath = %q, want empty when a token source is provided", adapter.tokenPath)
	}
}

func TestReadTokenFileIgnoresMalformedCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0600); err != nil {
		t.Fatal(err)
	}

	if token, ok := readTokenFile(path); ok || token != nil {
		t.Fatalf("readTokenFile(%q) = (%#v, %v), want (nil, false)", path, token, ok)
	}
}

func TestReadTokenFileLoadsValidCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := writeTokenFile(path, &oauth2.Token{AccessToken: "cached-token"}); err != nil {
		t.Fatal(err)
	}

	token, ok := readTokenFile(path)
	if !ok {
		t.Fatalf("readTokenFile(%q) ok = false, want true", path)
	}
	if token.AccessToken != "cached-token" {
		t.Fatalf("access token = %q, want cached-token", token.AccessToken)
	}
}
