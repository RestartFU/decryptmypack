// Package webaccounts keeps website visitors' Microsoft accounts.
//
// Each visitor is identified by a random secret held only in their browser cookie. Their accounts and
// tokens live in one AES-GCM vault file whose key is derived from both the server's master key and that
// secret, so the files on disk cannot be decrypted with the master key alone. The file name is a hash
// of the secret, so it does not reveal the cookie either.
package webaccounts

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

const (
	// MaxAccounts bounds how many Microsoft accounts one visitor can keep.
	MaxAccounts = 5
	// MaxPendingLogins bounds device logins waiting for approval across all visitors.
	MaxPendingLogins = 64

	secretBytes   = 32
	vaultVersion  = 1
	vaultSuffix   = ".vault"
	keyDerivation = "decryptmypack web account vault v1"
	loginLinger   = 2 * time.Minute
)

var (
	ErrAccountNotFound = errors.New("account not found")
	ErrLoginNotFound   = errors.New("sign-in not found or expired")
	ErrTooManyAccounts = fmt.Errorf("you can keep up to %d Microsoft accounts", MaxAccounts)
	ErrLoginsBusy      = errors.New("too many sign-ins are in progress; try again in a minute")
)

// Account is the public account model returned to the browser. It contains no credentials.
type Account struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"createdAt"`
}

// LoginChallenge is the user-visible part of a Microsoft device login.
type LoginChallenge struct {
	LoginID                 string    `json:"loginId"`
	UserCode                string    `json:"userCode"`
	VerificationURI         string    `json:"verificationUri"`
	VerificationURIComplete string    `json:"verificationUriComplete,omitempty"`
	ExpiresAt               time.Time `json:"expiresAt"`
}

// LoginState reports a device login's progress: pending, done, or failed.
type LoginState struct {
	State   string   `json:"state"`
	Account *Account `json:"account,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// Visitor is one browser's identity. The secret never leaves memory except in the visitor's cookie.
type Visitor struct {
	secret []byte
	id     string
}

// NewVisitor creates a fresh visitor and the cookie value that identifies it.
func NewVisitor() (Visitor, string, error) {
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Visitor{}, "", err
	}
	return visitorFromSecret(secret), base64.RawURLEncoding.EncodeToString(secret), nil
}

// ParseVisitor reads a visitor from its cookie value.
func ParseVisitor(cookie string) (Visitor, bool) {
	secret, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cookie))
	if err != nil || len(secret) != secretBytes {
		return Visitor{}, false
	}
	return visitorFromSecret(secret), true
}

func visitorFromSecret(secret []byte) Visitor {
	sum := sha256.Sum256(secret)
	return Visitor{secret: secret, id: hex.EncodeToString(sum[:])}
}

type storedAccount struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"createdAt"`
	Token     *oauth2.Token `json:"token"`
}

type vault struct {
	Version  int             `json:"version"`
	ActiveID string          `json:"activeId,omitempty"`
	Accounts []storedAccount `json:"accounts"`
}

type pendingLogin struct {
	visitor  Visitor
	state    LoginState
	cancel   context.CancelFunc
	finished time.Time
}

// Store owns every visitor vault and the device logins in flight.
type Store struct {
	dir           string
	master        []byte
	authenticator Authenticator

	mu     sync.Mutex
	logins map[string]*pendingLogin
}

// Open creates a store in dir using a 32-byte master key.
func Open(dir string, master []byte, authenticator Authenticator) (*Store, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("web account master key must be 32 bytes")
	}
	if authenticator == nil {
		return nil, fmt.Errorf("authenticator is required")
	}
	dir = filepath.Clean(dir)
	if err := ensurePrivateDir(dir); err != nil {
		return nil, fmt.Errorf("create web account storage: %w", err)
	}
	return &Store{dir: dir, master: master, authenticator: authenticator, logins: make(map[string]*pendingLogin)}, nil
}

// LoadOrCreateKey returns the master key from encoded (base64, 32 bytes) or from keyPath,
// creating a new private key file when neither exists.
func LoadOrCreateKey(encoded, keyPath string) ([]byte, error) {
	if encoded = strings.TrimSpace(encoded); encoded != "" {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("ACCOUNT_KEY must be 32 bytes encoded as base64")
		}
		return key, nil
	}
	body, err := os.ReadFile(keyPath)
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("web account key file %s is invalid", keyPath)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read web account key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := writePrivateFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)+"\n")); err != nil {
		return nil, fmt.Errorf("save web account key: %w", err)
	}
	return key, nil
}

// List returns the visitor's accounts in creation order. A visitor with no vault has none.
func (s *Store) List(v Visitor) ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked(v)
	if err != nil {
		return nil, err
	}
	return publicAccounts(data), nil
}

// BeginLogin starts a Microsoft device login for the visitor and finishes it in the background.
func (s *Store) BeginLogin(ctx context.Context, v Visitor) (LoginChallenge, error) {
	s.mu.Lock()
	data, err := s.loadLocked(v)
	if err != nil {
		s.mu.Unlock()
		return LoginChallenge{}, err
	}
	if len(data.Accounts) >= MaxAccounts {
		s.mu.Unlock()
		return LoginChallenge{}, ErrTooManyAccounts
	}
	s.pruneLoginsLocked()
	for id, login := range s.logins {
		if login.visitor.id == v.id && login.state.State == "pending" {
			login.cancel()
			delete(s.logins, id)
		}
	}
	if s.pendingCountLocked() >= MaxPendingLogins {
		s.mu.Unlock()
		return LoginChallenge{}, ErrLoginsBusy
	}
	s.mu.Unlock()

	challenge, err := s.authenticator.Begin(ctx)
	if err != nil {
		return LoginChallenge{}, fmt.Errorf("start Microsoft sign-in: %w", err)
	}
	if challenge == nil || challenge.DeviceCode == "" || challenge.UserCode == "" || challenge.VerificationURI == "" {
		return LoginChallenge{}, fmt.Errorf("Microsoft returned an incomplete sign-in code")
	}
	loginID, err := randomID()
	if err != nil {
		return LoginChallenge{}, err
	}
	deadline := challenge.Expiry
	if deadline.IsZero() {
		deadline = time.Now().Add(15 * time.Minute)
	}
	loginCtx, cancel := context.WithDeadline(context.Background(), deadline)
	login := &pendingLogin{visitor: v, state: LoginState{State: "pending"}, cancel: cancel}
	s.mu.Lock()
	s.logins[loginID] = login
	s.mu.Unlock()

	go s.completeLogin(loginCtx, cancel, login, challenge)

	return LoginChallenge{
		LoginID:                 loginID,
		UserCode:                challenge.UserCode,
		VerificationURI:         challenge.VerificationURI,
		VerificationURIComplete: challenge.VerificationURIComplete,
		ExpiresAt:               deadline,
	}, nil
}

// completeLogin waits for approval, saves the account, and records the outcome for polling.
func (s *Store) completeLogin(ctx context.Context, cancel context.CancelFunc, login *pendingLogin, challenge *oauth2.DeviceAuthResponse) {
	defer cancel()
	account, err := s.finishLogin(ctx, login.visitor, challenge)
	s.mu.Lock()
	defer s.mu.Unlock()
	login.finished = time.Now()
	if err != nil {
		message := "Microsoft sign-in did not finish."
		if errors.Is(err, ErrTooManyAccounts) {
			message = err.Error()
		} else if ctx.Err() != nil {
			message = "The sign-in code expired. Start again for a new one."
		}
		login.state = LoginState{State: "failed", Error: message}
		return
	}
	login.state = LoginState{State: "done", Account: &account}
}

func (s *Store) finishLogin(ctx context.Context, v Visitor, challenge *oauth2.DeviceAuthResponse) (Account, error) {
	token, err := s.authenticator.Complete(ctx, challenge)
	if err != nil {
		return Account{}, err
	}
	if token == nil || (token.AccessToken == "" && token.RefreshToken == "") {
		return Account{}, fmt.Errorf("Microsoft sign-in returned an empty token")
	}
	name, err := s.authenticator.AccountName(ctx, token)
	if err != nil {
		return Account{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 80 {
		return Account{}, fmt.Errorf("Xbox profile returned an unusable account name")
	}
	id, err := randomID()
	if err != nil {
		return Account{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked(v)
	if err != nil {
		return Account{}, err
	}
	if len(data.Accounts) >= MaxAccounts {
		return Account{}, ErrTooManyAccounts
	}
	stored := storedAccount{ID: id, Name: name, CreatedAt: time.Now().UTC(), Token: token}
	data.Accounts = append(data.Accounts, stored)
	data.ActiveID = id
	if err := s.saveLocked(v, data); err != nil {
		return Account{}, err
	}
	return Account{ID: id, Name: name, Active: true, CreatedAt: stored.CreatedAt}, nil
}

// LoginStatus reports a device login started by the same visitor.
func (s *Store) LoginStatus(v Visitor, loginID string) (LoginState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLoginsLocked()
	login, ok := s.logins[loginID]
	if !ok || login.visitor.id != v.id {
		return LoginState{}, ErrLoginNotFound
	}
	return login.state, nil
}

// CancelLogin stops a device login started by the same visitor.
func (s *Store) CancelLogin(v Visitor, loginID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if login, ok := s.logins[loginID]; ok && login.visitor.id == v.id {
		login.cancel()
		delete(s.logins, loginID)
	}
}

// Select makes one of the visitor's accounts the default for new downloads.
func (s *Store) Select(v Visitor, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked(v)
	if err != nil {
		return err
	}
	if indexOf(data, accountID) < 0 {
		return ErrAccountNotFound
	}
	data.ActiveID = accountID
	return s.saveLocked(v, data)
}

// Remove deletes one of the visitor's accounts and its token. An empty vault is deleted from disk.
func (s *Store) Remove(v Visitor, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked(v)
	if err != nil {
		return err
	}
	index := indexOf(data, accountID)
	if index < 0 {
		return ErrAccountNotFound
	}
	data.Accounts = append(data.Accounts[:index], data.Accounts[index+1:]...)
	if data.ActiveID == accountID {
		data.ActiveID = ""
		if len(data.Accounts) > 0 {
			data.ActiveID = data.Accounts[0].ID
		}
	}
	if len(data.Accounts) == 0 {
		if err := os.Remove(s.vaultPath(v)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return s.saveLocked(v, data)
}

// TokenSource returns a refreshing token source for one of the visitor's accounts.
// Refreshed tokens are written back into the visitor's vault.
func (s *Store) TokenSource(v Visitor, accountID string) (oauth2.TokenSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := s.loadLocked(v)
	if err != nil {
		return nil, err
	}
	index := indexOf(data, accountID)
	if index < 0 {
		return nil, ErrAccountNotFound
	}
	token := data.Accounts[index].Token
	if token == nil {
		return nil, ErrAccountNotFound
	}
	return &vaultTokenSource{store: s, visitor: v, accountID: accountID, source: s.authenticator.Refresh(token)}, nil
}

// Prune deletes vaults not used for maxAge, so abandoned browsers do not keep tokens forever.
func (s *Store) Prune(maxAge time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-maxAge)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), vaultSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(s.dir, entry.Name()))
	}
	return nil
}

type vaultTokenSource struct {
	mu        sync.Mutex
	store     *Store
	visitor   Visitor
	accountID string
	source    oauth2.TokenSource
	last      string
}

// Token refreshes when needed and saves a changed token back into the vault.
func (t *vaultTokenSource) Token() (*oauth2.Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	token, err := t.source.Token()
	if err != nil {
		return nil, err
	}
	fingerprint := token.AccessToken + "\x00" + token.RefreshToken
	if fingerprint == t.last {
		return token, nil
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	data, err := t.store.loadLocked(t.visitor)
	if err != nil {
		return nil, err
	}
	index := indexOf(data, t.accountID)
	if index < 0 {
		return nil, ErrAccountNotFound
	}
	data.Accounts[index].Token = token
	if err := t.store.saveLocked(t.visitor, data); err != nil {
		return nil, fmt.Errorf("save refreshed account token: %w", err)
	}
	t.last = fingerprint
	return token, nil
}

func (s *Store) vaultPath(v Visitor) string {
	return filepath.Join(s.dir, v.id+vaultSuffix)
}

func (s *Store) key(v Visitor) []byte {
	mac := hmac.New(sha256.New, s.master)
	mac.Write([]byte(keyDerivation))
	mac.Write(v.secret)
	return mac.Sum(nil)
}

func (s *Store) loadLocked(v Visitor) (vault, error) {
	path := s.vaultPath(v)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return vault{Version: vaultVersion, Accounts: []storedAccount{}}, nil
	}
	if err != nil {
		return vault{}, fmt.Errorf("read account vault: %w", err)
	}
	aead, err := s.aead(v)
	if err != nil {
		return vault{}, err
	}
	if len(body) < aead.NonceSize() {
		return vault{}, fmt.Errorf("account vault is damaged")
	}
	plain, err := aead.Open(nil, body[:aead.NonceSize()], body[aead.NonceSize():], []byte(v.id))
	if err != nil {
		return vault{}, fmt.Errorf("account vault could not be decrypted")
	}
	var data vault
	if err := json.Unmarshal(plain, &data); err != nil || data.Version != vaultVersion {
		return vault{}, fmt.Errorf("account vault is damaged")
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return data, nil
}

func (s *Store) saveLocked(v Visitor, data vault) error {
	data.Version = vaultVersion
	plain, err := json.Marshal(data)
	if err != nil {
		return err
	}
	aead, err := s.aead(v)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	return writePrivateFile(s.vaultPath(v), aead.Seal(nonce, nonce, plain, []byte(v.id)))
}

func (s *Store) aead(v Visitor) (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key(v))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Store) pendingCountLocked() int {
	count := 0
	for _, login := range s.logins {
		if login.state.State == "pending" {
			count++
		}
	}
	return count
}

// pruneLoginsLocked forgets finished logins once their result has had time to be read.
func (s *Store) pruneLoginsLocked() {
	for id, login := range s.logins {
		if !login.finished.IsZero() && time.Since(login.finished) > loginLinger {
			delete(s.logins, id)
		}
	}
}

func publicAccounts(data vault) []Account {
	result := make([]Account, 0, len(data.Accounts))
	for _, stored := range data.Accounts {
		result = append(result, Account{ID: stored.ID, Name: stored.Name, Active: stored.ID == data.ActiveID, CreatedAt: stored.CreatedAt})
	}
	return result
}

func indexOf(data vault, accountID string) int {
	for i, stored := range data.Accounts {
		if stored.ID == accountID {
			return i
		}
	}
	return -1
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func writePrivateFile(path string, body []byte) error {
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
