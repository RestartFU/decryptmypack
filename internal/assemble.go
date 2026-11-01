package internal

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/restartfu/decryptmypack/internal/adapters/downloader"
	"github.com/restartfu/decryptmypack/internal/adapters/filesystem"
	"github.com/restartfu/decryptmypack/internal/config"
	"github.com/restartfu/decryptmypack/internal/service"
	"github.com/restartfu/decryptmypack/internal/webaccounts"
)

// webAccountMaxIdle is how long an unused visitor vault is kept before its tokens are deleted.
const webAccountMaxIdle = 180 * 24 * time.Hour

func Assemble(config config.Config) (*service.Service, error) {
	store, err := filesystem.New(config.DataDir)
	if err != nil {
		return nil, err
	}
	accountDir := filepath.Join(config.DataDir, "web-accounts")
	key, err := webaccounts.LoadOrCreateKey(config.AccountKey, filepath.Join(config.DataDir, "web-accounts.key"))
	if err != nil {
		return nil, err
	}
	webAccounts, err := webaccounts.Open(accountDir, key, webaccounts.LiveAuthenticator())
	if err != nil {
		return nil, err
	}
	_ = webAccounts.Prune(webAccountMaxIdle)
	options := []service.Option{
		service.WithMaxConcurrentDownloads(config.MaxConcurrentDownloads),
		service.WithWebAccounts(webAccounts),
	}
	if _, err := os.Stat(config.TokenPath); err != nil {
		log.Printf("no service token at %s: servers that are not cached yet need a visitor's Microsoft account", config.TokenPath)
		options = append(options, service.WithoutServiceAccount())
	}
	return service.New(store, downloader.New(config.TokenPath), options...), nil
}
