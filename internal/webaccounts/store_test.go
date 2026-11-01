package webaccounts

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type fakeAuthenticator struct {
	approve   chan struct{}
	token     *oauth2.Token
	refreshed *oauth2.Token
	name      string
}

func (f *fakeAuthenticator) Begin(context.Context) (*oauth2.DeviceAuthResponse, error) {
	return &oauth2.DeviceAuthResponse{
		DeviceCode:      "device-secret",
		UserCode:        "ABCD-EFGH",
		VerificationURI: "https://microsoft.com/link",
		Expiry:          time.Now().Add(10 * time.Minute),
	}, nil
}

func (f *fakeAuthenticator) Complete(ctx context.Context, _ *oauth2.DeviceAuthResponse) (*oauth2.Token, error) {
	if f.approve != nil {
		select {
		case <-f.approve:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.token, nil
}

func (f *fakeAuthenticator) AccountName(context.Context, *oauth2.Token) (string, error) {
	return f.name, nil
}

func (f *fakeAuthenticator) Refresh(*oauth2.Token) oauth2.TokenSource {
	return oauth2.StaticTokenSource(f.refreshed)
}

func newTestStore(t *testing.T, authenticator *fakeAuthenticator) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	key := bytes.Repeat([]byte{7}, 32)
	store, err := Open(dir, key, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func testAuthenticator() *fakeAuthenticator {
	return &fakeAuthenticator{
		token:     &oauth2.Token{AccessToken: "access-secret", RefreshToken: "refresh-secret", Expiry: time.Now().Add(time.Hour)},
		refreshed: &oauth2.Token{AccessToken: "fresh-secret", RefreshToken: "refresh-secret-2", Expiry: time.Now().Add(time.Hour)},
		name:      "Steve Builder",
	}
}

// waitForLogin polls a login until it leaves the pending state.
func waitForLogin(t *testing.T, store *Store, v Visitor, loginID string) LoginState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := store.LoginStatus(v, loginID)
		if err != nil {
			t.Fatal(err)
		}
		if state.State != "pending" {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("login never finished")
	return LoginState{}
}

func TestLoginSavesEncryptedAccountScopedToVisitor(t *testing.T) {
	store, dir := newTestStore(t, testAuthenticator())
	visitor, cookie, err := NewVisitor()
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := store.BeginLogin(context.Background(), visitor)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "ABCD-EFGH" || challenge.LoginID == "" {
		t.Fatalf("challenge = %+v", challenge)
	}
	state := waitForLogin(t, store, visitor, challenge.LoginID)
	if state.State != "done" || state.Account == nil || state.Account.Name != "Steve Builder" || !state.Account.Active {
		t.Fatalf("state = %+v", state)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one vault file, found %d", len(entries))
	}
	name := entries[0].Name()
	if strings.Contains(name, cookie) {
		t.Fatal("vault file name reveals the visitor cookie")
	}
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "Steve Builder"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("vault stores %q in plaintext", secret)
		}
	}
	if info, _ := os.Stat(filepath.Join(dir, name)); info.Mode().Perm() != 0o600 {
		t.Fatalf("vault mode = %v", info.Mode().Perm())
	}

	parsed, ok := ParseVisitor(cookie)
	if !ok {
		t.Fatal("cookie did not parse")
	}
	list, err := store.List(parsed)
	if err != nil || len(list) != 1 || list[0].Name != "Steve Builder" {
		t.Fatalf("list = %+v, %v", list, err)
	}

	stranger, _, _ := NewVisitor()
	if list, err := store.List(stranger); err != nil || len(list) != 0 {
		t.Fatalf("another visitor sees %+v, %v", list, err)
	}
	if _, err := store.LoginStatus(stranger, challenge.LoginID); err != ErrLoginNotFound {
		t.Fatalf("another visitor read the login state: %v", err)
	}
	if _, err := store.TokenSource(stranger, list[0].ID); err == nil {
		t.Fatal("another visitor obtained a token source")
	}
}

func TestVaultNeedsBothMasterKeyAndVisitorSecret(t *testing.T) {
	authenticator := testAuthenticator()
	store, dir := newTestStore(t, authenticator)
	visitor, _, _ := NewVisitor()
	challenge, err := store.BeginLogin(context.Background(), visitor)
	if err != nil {
		t.Fatal(err)
	}
	waitForLogin(t, store, visitor, challenge.LoginID)

	otherKey, err := Open(dir, bytes.Repeat([]byte{9}, 32), authenticator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherKey.List(visitor); err == nil {
		t.Fatal("vault decrypted with the wrong master key")
	}
}

func TestTokenSourceRefreshesIntoVault(t *testing.T) {
	store, _ := newTestStore(t, testAuthenticator())
	visitor, _, _ := NewVisitor()
	challenge, _ := store.BeginLogin(context.Background(), visitor)
	state := waitForLogin(t, store, visitor, challenge.LoginID)

	source, err := store.TokenSource(visitor, state.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token()
	if err != nil || token.AccessToken != "fresh-secret" {
		t.Fatalf("token = %+v, %v", token, err)
	}
	store.mu.Lock()
	data, err := store.loadLocked(visitor)
	store.mu.Unlock()
	if err != nil || data.Accounts[0].Token.RefreshToken != "refresh-secret-2" {
		t.Fatalf("refreshed token was not saved: %+v, %v", data.Accounts, err)
	}
}

func TestSelectRemoveAndAccountLimit(t *testing.T) {
	store, dir := newTestStore(t, testAuthenticator())
	visitor, _, _ := NewVisitor()
	var ids []string
	for i := 0; i < MaxAccounts; i++ {
		challenge, err := store.BeginLogin(context.Background(), visitor)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, waitForLogin(t, store, visitor, challenge.LoginID).Account.ID)
	}
	if _, err := store.BeginLogin(context.Background(), visitor); err != ErrTooManyAccounts {
		t.Fatalf("expected account limit, got %v", err)
	}
	if err := store.Select(visitor, ids[1]); err != nil {
		t.Fatal(err)
	}
	list, _ := store.List(visitor)
	if !list[1].Active || list[0].Active {
		t.Fatalf("select did not change the active account: %+v", list)
	}
	for _, id := range ids {
		if err := store.Remove(visitor, id); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("empty vault was not deleted: %d files remain", len(entries))
	}
}

func TestCancelledLoginNeverSavesAccount(t *testing.T) {
	authenticator := testAuthenticator()
	authenticator.approve = make(chan struct{})
	store, _ := newTestStore(t, authenticator)
	visitor, _, _ := NewVisitor()
	challenge, err := store.BeginLogin(context.Background(), visitor)
	if err != nil {
		t.Fatal(err)
	}
	store.CancelLogin(visitor, challenge.LoginID)
	if _, err := store.LoginStatus(visitor, challenge.LoginID); err != ErrLoginNotFound {
		t.Fatalf("cancelled login still reported: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if list, _ := store.List(visitor); len(list) != 0 {
		t.Fatalf("cancelled login saved %+v", list)
	}
}

func TestParseVisitorRejectsMalformedCookies(t *testing.T) {
	for _, cookie := range []string{"", "short", strings.Repeat("A", 200), "!!!!"} {
		if _, ok := ParseVisitor(cookie); ok {
			t.Fatalf("cookie %q parsed", cookie)
		}
	}
}

func TestLoadOrCreateKeyPersistsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	first, err := LoadOrCreateKey("", path)
	if err != nil || len(first) != 32 {
		t.Fatalf("key = %d bytes, %v", len(first), err)
	}
	second, err := LoadOrCreateKey("", path)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("key file was not reused")
	}
	if _, err := LoadOrCreateKey("not-base64", path); err == nil {
		t.Fatal("invalid ACCOUNT_KEY accepted")
	}
}
