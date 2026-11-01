package tests

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/restartfu/decryptmypack/internal/adapters/filesystem"
	"github.com/restartfu/decryptmypack/internal/service"
)

type e2eApp struct {
	http       *httptest.Server
	dataDir    string
	downloader *fakeDownloader
}

func startE2E(t *testing.T, downloader *fakeDownloader) *e2eApp {
	t.Helper()

	if downloader == nil {
		downloader = &fakeDownloader{body: testPackArchive(t, nil)}
	}
	dataDir := t.TempDir()
	store, err := filesystem.New(dataDir)
	if err != nil {
		t.Fatalf("create filesystem store: %v", err)
	}
	httpServer := httptest.NewServer(service.New(store, downloader).Handler())
	t.Cleanup(httpServer.Close)

	return &e2eApp{
		http:       httpServer,
		dataDir:    dataDir,
		downloader: downloader,
	}
}

func (a *e2eApp) downloadURL(target string) string {
	return a.http.URL + "/download?target=" + url.QueryEscape(target)
}

func (a *e2eApp) statusURL(target string) string {
	return a.http.URL + "/status?target=" + url.QueryEscape(target)
}

func packURLFor(target string) string {
	host, port := parseTestTarget(target)
	return "/packs/" + host + "/" + strconv.Itoa(port) + "/" + host + ".zip"
}

func packPathFor(dataDir, target string) string {
	host, port := parseTestTarget(target)
	return filepath.Join(dataDir, "packs", host, strconv.Itoa(port), host+".zip")
}

func iconURLFor(target string) string {
	host, port := parseTestTarget(target)
	return "/icons/" + host + "/" + strconv.Itoa(port) + "/icon.png"
}

func iconPathFor(dataDir, target string) string {
	host, port := parseTestTarget(target)
	return filepath.Join(dataDir, "icons", host, strconv.Itoa(port), "icon.png")
}

func parseTestTarget(target string) (string, int) {
	host := strings.TrimSpace(target)
	port := 19132
	if splitHost, splitPort, ok := strings.Cut(host, ":"); ok {
		host = strings.TrimSpace(splitHost)
		if parsed, err := strconv.Atoi(strings.TrimSpace(splitPort)); err == nil {
			port = parsed
		}
	}
	return strings.ToLower(host), port
}

func testPackArchive(t *testing.T, icon []byte) []byte {
	t.Helper()

	var inner bytes.Buffer
	innerZip := zip.NewWriter(&inner)
	writeTestZipFile(t, innerZip, "manifest.json", []byte(`{"format_version":2}`))
	if len(icon) > 0 {
		writeTestZipFile(t, innerZip, "pack_icon.png", icon)
	}
	if err := innerZip.Close(); err != nil {
		t.Fatalf("close inner zip: %v", err)
	}

	var outer bytes.Buffer
	outerZip := zip.NewWriter(&outer)
	writeTestZipFile(t, outerZip, "Example Pack.zip", inner.Bytes())
	if err := outerZip.Close(); err != nil {
		t.Fatalf("close outer zip: %v", err)
	}
	return outer.Bytes()
}

func writeTestZipFile(t *testing.T, zw *zip.Writer, name string, body []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("create %s in test zip: %v", name, err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatalf("write %s in test zip: %v", name, err)
	}
}

type fakeDownloader struct {
	body    []byte
	err     error
	started chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (d *fakeDownloader) DownloadFrom(_, _ string, _ int, progress func(string)) (io.Reader, error) {
	d.calls.Add(1)
	if progress != nil {
		progress("downloading test pack")
	}
	if d.started != nil {
		select {
		case d.started <- struct{}{}:
		default:
		}
	}
	if d.release != nil {
		<-d.release
	}
	if d.err != nil {
		return nil, d.err
	}
	return bytes.NewReader(d.body), nil
}

func readResponse(t *testing.T, rawURL string) (int, string, http.Header) {
	t.Helper()

	response, err := http.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response from %s: %v", rawURL, err)
	}
	return response.StatusCode, string(body), response.Header.Clone()
}

func pollUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
