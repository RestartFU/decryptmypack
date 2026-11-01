package downloader

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sandertv/gophertunnel/minecraft/auth"
	"golang.org/x/oauth2"
)

var (
	sources   = make(map[string]oauth2.TokenSource)
	sourcesMu sync.Mutex
)

func tokenSource(path string) (oauth2.TokenSource, error) {
	path = cleanTokenPath(path)
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if source := sources[path]; source != nil {
		return source, nil
	}
	source, err := newTokenSource(path)
	if err != nil {
		return nil, err
	}
	sources[path] = source
	return source, nil
}

func EnsureTokenSource() error {
	_, err := tokenSource("token.json")
	return err
}

// Login creates or refreshes the service token at path, printing a Microsoft device code when a new
// sign-in is needed.
func Login(path string) error {
	_, err := newTokenSource(path)
	return err
}

func newTokenSource(path string) (oauth2.TokenSource, error) {
	path = cleanTokenPath(path)
	token, ok := readTokenFile(path)
	var err error
	if !ok {
		token, err = auth.RequestLiveToken()
		if err != nil {
			return nil, fmt.Errorf("request live token: %w", err)
		}
		if err := writeTokenFile(path, token); err != nil {
			return nil, fmt.Errorf("save live token: %w", err)
		}
	}

	refreshSource := auth.RefreshTokenSource(token)
	refreshed, err := refreshSource.Token()
	if err != nil {
		refreshErr := err
		token, err = auth.RequestLiveToken()
		if err != nil {
			return nil, fmt.Errorf("cached token refresh failed (%v), request live token: %w", refreshErr, err)
		}
		if err := writeTokenFile(path, token); err != nil {
			return nil, fmt.Errorf("save replacement live token: %w", err)
		}
		refreshSource = auth.RefreshTokenSource(token)
		refreshed, err = refreshSource.Token()
		if err != nil {
			return nil, fmt.Errorf("refresh replacement live token: %w", err)
		}
	}
	if err := writeTokenFile(path, refreshed); err != nil {
		return nil, fmt.Errorf("save refreshed token: %w", err)
	}
	return &persistingTokenSource{source: refreshSource, path: path}, nil
}

type persistingTokenSource struct {
	mu     sync.Mutex
	source oauth2.TokenSource
	path   string
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, err := s.source.Token()
	if err != nil {
		return nil, err
	}
	if err := writeTokenFile(s.path, token); err != nil {
		return nil, fmt.Errorf("save refreshed token: %w", err)
	}
	return token, nil
}

func cleanTokenPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "token.json"
	}
	return filepath.Clean(path)
}

func readTokenFile(path string) (*oauth2.Token, bool) {
	tokenData, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false
	}
	if err != nil {
		log.Printf("could not read token cache %s: %v", path, err)
		return nil, false
	}
	token := new(oauth2.Token)
	if err := json.Unmarshal(tokenData, token); err != nil {
		log.Printf("ignoring invalid token cache %s: %v", path, err)
		return nil, false
	}
	return token, true
}

func writeTokenFile(path string, token *oauth2.Token) error {
	path = cleanTokenPath(path)
	directory := filepath.Dir(path)
	if directory != "." {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	body, err := json.Marshal(token)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
