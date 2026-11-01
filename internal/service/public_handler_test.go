package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/restartfu/decryptmypack/internal/adapters/filesystem"
)

type publicTestDownloader struct {
	body    []byte
	started chan struct{}
	release chan struct{}
	targets chan [2]string
	calls   atomic.Int64
}

func (d *publicTestDownloader) DownloadFrom(host, resolvedIP string, _ int, progress func(string)) (io.Reader, error) {
	d.calls.Add(1)
	if d.targets != nil {
		d.targets <- [2]string{host, resolvedIP}
	}
	if progress != nil {
		progress("downloading pack 50%")
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
	return bytes.NewReader(d.body), nil
}

func TestPublicHandlerPinsResolvedAddressBeforeDownloader(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	downloader := &publicTestDownloader{
		body:    testOuterArchive(t, map[string][]byte{"Pack.zip": []byte("pack")}),
		targets: make(chan [2]string, 1),
	}
	handler := New(store, downloader, withTargetResolver(func(host string) (string, error) {
		if host != "example.test" {
			t.Fatalf("resolver host = %q", host)
		}
		return "8.8.4.4", nil
	})).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/download?host=example.test", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("download status = %d", response.Code)
	}
	select {
	case got := <-downloader.targets:
		if got != [2]string{"example.test", "8.8.4.4"} {
			t.Fatalf("downloader target = %q, want logical host and pinned IP", got)
		}
	case <-time.After(time.Second):
		t.Fatal("downloader did not start")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status := httptest.NewRecorder()
		handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/status?host=example.test", nil))
		if strings.Contains(status.Body.String(), `"status":"ready"`) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("download did not finish")
}

func TestCachedHostnameDoesNotRequireDNS(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload("offline.invalid", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	resolverCalled := false
	handler := New(store, &publicTestDownloader{}, withTargetResolver(func(string) (string, error) {
		resolverCalled = true
		return "", io.ErrUnexpectedEOF
	})).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/download?host=offline.invalid", nil))
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "/packs/offline.invalid/19132/offline.invalid.zip" {
		t.Fatalf("cached download = %d %q", response.Code, response.Body.String())
	}
	if resolverCalled {
		t.Fatal("cached hostname triggered DNS resolution")
	}
}

func TestStartContextFlushesPendingStatsOnShutdown(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload("example.test", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.test", 19132); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(store, &publicTestDownloader{}).StartContext(ctx, "127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Stats().SuccessfulDownloads; got != 1 {
		t.Fatalf("persisted downloads = %d, want 1", got)
	}
}

func TestPublicHandlerDownloadsAndServesPackWithoutAuth(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	downloader := &publicTestDownloader{body: testOuterArchive(t, map[string][]byte{
		"Example Pack.zip": []byte("inner pack"),
	})}
	server := httptest.NewServer(New(store, downloader).Handler())
	defer server.Close()

	downloadURL := server.URL + "/download?host=8.8.8.8&port=19132"
	response, err := http.Get(downloadURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted || string(body) != "processing" {
		t.Fatalf("first download response = %d %q", response.StatusCode, body)
	}

	var packURL string
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		response, err = http.Get(downloadURL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ = io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode == http.StatusOK {
			packURL = strings.TrimSpace(string(body))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if packURL != "/packs/8.8.8.8/19132/8.8.8.8.zip" {
		t.Fatalf("pack URL = %q", packURL)
	}

	response, err = http.Get(server.URL + packURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pack response = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Disposition"); got != `attachment; filename="8.8.8.8.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

func TestPublicHandlerExposesHealthStatsAndBrowse(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload("example.com", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.com", 19132); err != nil {
		t.Fatal(err)
	}
	handler := New(store, &publicTestDownloader{}).Handler()

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"storage":"local"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}

	statsResponse := httptest.NewRecorder()
	handler.ServeHTTP(statsResponse, httptest.NewRequest(http.MethodGet, "/stats", nil))
	var stats map[string]int
	if err := json.Unmarshal(statsResponse.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats["successful_downloads"] != 1 || stats["unique_successful_targets"] != 1 {
		t.Fatalf("stats = %#v", stats)
	}

	browseResponse := httptest.NewRecorder()
	handler.ServeHTTP(browseResponse, httptest.NewRequest(http.MethodGet, "/browse?q=example", nil))
	if browseResponse.Code != http.StatusOK {
		t.Fatalf("browse status = %d", browseResponse.Code)
	}
	if body := browseResponse.Body.String(); !strings.Contains(body, `"host":"example.com"`) || !strings.Contains(body, `"cache_state":"cached"`) {
		t.Fatalf("browse = %s", body)
	}
}

func TestPublicHandlerRejectsPrivateTarget(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, &publicTestDownloader{}).Handler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/download?host=127.0.0.1", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("private target status = %d, want 400", response.Code)
	}
	if !strings.Contains(strings.ToLower(response.Body.String()), "public") {
		t.Fatalf("private target body = %q", response.Body.String())
	}
}

func TestPublicHandlerBoundsConcurrentDownloads(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	downloader := &publicTestDownloader{
		body:    testOuterArchive(t, map[string][]byte{"Pack.zip": []byte("pack")}),
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	handler := New(store, downloader, WithMaxConcurrentDownloads(1)).Handler()

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/download?host=8.8.8.8", nil))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status = %d", first.Code)
	}
	select {
	case <-downloader.started:
	case <-time.After(time.Second):
		t.Fatal("first download did not start")
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/download?host=1.1.1.1", nil))
	if second.Code != http.StatusServiceUnavailable {
		t.Fatalf("second status = %d, want 503", second.Code)
	}
	close(downloader.release)
}
