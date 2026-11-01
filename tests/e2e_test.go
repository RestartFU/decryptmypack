package tests

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/restartfu/decryptmypack/internal"
	"github.com/restartfu/decryptmypack/internal/config"
	"github.com/restartfu/decryptmypack/internal/ports"
)

func TestAssembledAppServesPublicFrontendAndHealth(t *testing.T) {
	dataDir := t.TempDir()
	svc, err := internal.Assemble(config.Config{
		DataDir:                dataDir,
		TokenPath:              filepath.Join(t.TempDir(), "token.json"),
		MaxConcurrentDownloads: 1,
	})
	if err != nil {
		t.Fatalf("assemble app: %v", err)
	}
	server := httptest.NewServer(svc.Handler())
	defer server.Close()

	status, body, _ := readResponse(t, server.URL+"/")
	if status != http.StatusOK {
		t.Fatalf("frontend status = %d, want 200", status)
	}
	if !strings.Contains(body, "decrypt my pack") || strings.Contains(strings.ToLower(body), "sign in") {
		t.Fatalf("frontend does not look like the public anonymous page")
	}

	status, body, _ = readResponse(t, server.URL+"/health")
	if status != http.StatusOK {
		t.Fatalf("health status = %d, want 200", status)
	}
	var health map[string]any
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if health["ok"] != true || health["storage"] != "local" {
		t.Fatalf("health response = %#v", health)
	}
}

func TestAnonymousDownloadPersistsAndServesPack(t *testing.T) {
	const target = "8.8.8.8"
	archive := testPackArchive(t, nil)
	app := startE2E(t, &fakeDownloader{body: archive})

	status, body, _ := readResponse(t, app.downloadURL(target))
	if status != http.StatusAccepted || body != "processing" {
		t.Fatalf("initial download response = %d %q, want 202 processing", status, body)
	}

	pollUntil(t, time.Second, func() bool {
		status, body, _ = readResponse(t, app.downloadURL(target))
		return status == http.StatusOK
	})
	if body != packURLFor(target) {
		t.Fatalf("download URL = %q, want %q", body, packURLFor(target))
	}
	if got := app.downloader.calls.Load(); got != 1 {
		t.Fatalf("downloader calls = %d, want 1", got)
	}

	persisted, err := os.ReadFile(packPathFor(app.dataDir, target))
	if err != nil {
		t.Fatalf("read persisted pack: %v", err)
	}
	if !bytes.Equal(persisted, archive) {
		t.Fatal("persisted pack differs from downloaded archive")
	}

	status, body, headers := readResponse(t, app.http.URL+packURLFor(target))
	if status != http.StatusOK || !bytes.Equal([]byte(body), archive) {
		t.Fatalf("served pack response = %d with %d bytes", status, len(body))
	}
	if got := headers.Get("Content-Type"); got != "application/zip" {
		t.Fatalf("pack Content-Type = %q, want application/zip", got)
	}
	if got := headers.Get("Content-Disposition"); got != `attachment; filename="8.8.8.8.zip"` {
		t.Fatalf("pack Content-Disposition = %q", got)
	}

	status, body, _ = readResponse(t, app.http.URL+"/stats")
	if status != http.StatusOK {
		t.Fatalf("stats status = %d", status)
	}
	var stats ports.DownloadStats
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.SuccessfulDownloads != 1 || stats.UniqueSuccessfulTargets != 1 {
		t.Fatalf("stats = %#v, want one download of one target", stats)
	}
}

func TestTargetParameterPersistsAndServesPackIcon(t *testing.T) {
	const target = "1.1.1.1:19133"
	icon := validTestPNG(t)
	app := startE2E(t, &fakeDownloader{body: testPackArchive(t, icon)})

	status, body, _ := readResponse(t, app.downloadURL(target))
	if status != http.StatusAccepted || body != "processing" {
		t.Fatalf("initial download response = %d %q, want 202 processing", status, body)
	}
	pollUntil(t, time.Second, func() bool {
		status, body, _ = readResponse(t, app.statusURL(target))
		return status == http.StatusOK && strings.Contains(body, `"status":"ready"`)
	})

	persisted, err := os.ReadFile(iconPathFor(app.dataDir, target))
	if err != nil {
		t.Fatalf("read persisted icon: %v", err)
	}
	if !bytes.Equal(persisted, icon) {
		t.Fatal("persisted icon differs from pack icon")
	}

	status, body, headers := readResponse(t, app.http.URL+iconURLFor(target))
	if status != http.StatusOK || !bytes.Equal([]byte(body), icon) {
		t.Fatalf("served icon response = %d with %d bytes", status, len(body))
	}
	if !strings.HasPrefix(headers.Get("Content-Type"), "image/png") {
		t.Fatalf("icon Content-Type = %q, want image/png", headers.Get("Content-Type"))
	}
}

func validTestPNG(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	pixel := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	pixel.Set(0, 0, color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff})
	if err := png.Encode(&body, pixel); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestDuplicateRequestsShareOneInFlightDownload(t *testing.T) {
	const target = "8.8.4.4"
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	app := startE2E(t, &fakeDownloader{
		body:    testPackArchive(t, nil),
		started: started,
		release: release,
	})

	status, body, _ := readResponse(t, app.downloadURL(target))
	if status != http.StatusAccepted || body != "processing" {
		t.Fatalf("first response = %d %q, want 202 processing", status, body)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}

	status, body, _ = readResponse(t, app.downloadURL(target))
	if status != http.StatusAccepted || body != "processing" {
		t.Fatalf("second response = %d %q, want 202 processing", status, body)
	}
	if got := app.downloader.calls.Load(); got != 1 {
		t.Fatalf("downloader calls while processing = %d, want 1", got)
	}
	close(release)
	released = true
	pollUntil(t, time.Second, func() bool {
		status, _, _ := readResponse(t, app.statusURL(target))
		if status != http.StatusOK {
			return false
		}
		_, err := os.Stat(packPathFor(app.dataDir, target))
		return err == nil
	})
}

func TestDownloadFailureIsReportedWithoutLiveNetwork(t *testing.T) {
	const target = "9.9.9.9"
	app := startE2E(t, &fakeDownloader{err: errors.New("test download failed")})

	status, body, _ := readResponse(t, app.downloadURL(target))
	if status != http.StatusAccepted || body != "processing" {
		t.Fatalf("initial download response = %d %q, want 202 processing", status, body)
	}
	pollUntil(t, time.Second, func() bool {
		status, body, _ = readResponse(t, app.statusURL(target))
		return status == http.StatusOK && strings.Contains(body, `"status":"failed"`)
	})
	if !strings.Contains(body, "test download failed") {
		t.Fatalf("failure status body = %q", body)
	}
}

func TestPrivateTargetIsRejectedBeforeDownloaderRuns(t *testing.T) {
	app := startE2E(t, &fakeDownloader{body: testPackArchive(t, nil)})

	status, body, _ := readResponse(t, app.downloadURL("127.0.0.1"))
	if status != http.StatusBadRequest || !strings.Contains(strings.ToLower(body), "public") {
		t.Fatalf("private target response = %d %q, want public-address error", status, body)
	}
	if got := app.downloader.calls.Load(); got != 0 {
		t.Fatalf("downloader calls = %d, want 0", got)
	}
}
