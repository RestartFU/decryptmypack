package ports

import (
	"io"
	"net/http"

	"golang.org/x/oauth2"
)

type DownloadStats struct {
	SuccessfulDownloads     int64 `json:"successful_downloads"`
	UniqueSuccessfulTargets int   `json:"unique_successful_targets"`
}

type Server struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Downloads        int64  `json:"downloads"`
	LastDownloadedAt string `json:"last_downloaded_at,omitempty"`
}

type Store interface {
	Upload(ip string, port int, packs io.Reader) error
	UploadIcon(ip string, port int, icon []byte, contentType string) error
	FetchDownloadURL(ip string, port int) (string, error)
	RecordDownload(ip string, port int) error
	Flush() error
	Stats() DownloadStats
	Servers(query string) []Server
	Handler() http.Handler
}

type Downloader interface {
	DownloadFrom(host, resolvedIP string, port int, progress func(string)) (io.Reader, error)
}

// AccountDownloader downloads with a visitor's own Microsoft account instead of the service token.
type AccountDownloader interface {
	DownloadWithTokenSource(source oauth2.TokenSource, host, resolvedIP string, port int, progress func(string)) (io.Reader, error)
}
