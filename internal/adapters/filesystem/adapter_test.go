package filesystem

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPNG(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	icon := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	icon.Set(0, 0, color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff})
	if err := png.Encode(&body, icon); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	icon := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	icon.Set(0, 0, color.NRGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xff})
	if err := jpeg.Encode(&body, icon, nil); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func TestNewRejectsBlankRoot(t *testing.T) {
	if _, err := New("   "); err == nil {
		t.Fatal("New accepted a blank data directory")
	}
}

func TestNewMakesRelativeRootAbsoluteAndServesFromDot(t *testing.T) {
	workingDirectory := t.TempDir()
	previousDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDirectory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	store, err := New(".")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(store.root) {
		t.Fatalf("store root = %q, want an absolute path", store.root)
	}
	if err := store.Upload("example.com", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/packs/example.com/19132/example.com.zip", nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET pack status = %d, want 200", response.Code)
	}
}

func TestAdapterStoresAndServesPack(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pack := []byte("zip payload")
	if err := store.Upload("Example.COM", 19132, bytes.NewReader(pack)); err != nil {
		t.Fatal(err)
	}

	gotURL, err := store.FetchDownloadURL("example.com", 19132)
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != "/packs/example.com/19132/example.com.zip" {
		t.Fatalf("download URL = %q", gotURL)
	}

	request := httptest.NewRequest(http.MethodGet, gotURL, nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET pack status = %d, want 200", response.Code)
	}
	if !bytes.Equal(response.Body.Bytes(), pack) {
		t.Fatalf("GET pack body = %q", response.Body.Bytes())
	}
	if got := response.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="example.com.zip"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

func TestUploadIconRejectsContentTypeMismatch(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UploadIcon("example.com", 19132, testPNG(t), "image/jpeg"); err == nil {
		t.Fatal("UploadIcon accepted PNG bytes declared as image/jpeg")
	}
}

func TestUploadIconRejectsOversizedBody(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	icon := make([]byte, 3<<20)
	copy(icon, testPNG(t))
	if err := store.UploadIcon("example.com", 19132, icon, "image/png"); err == nil {
		t.Fatal("UploadIcon accepted a 3 MiB icon")
	}
}

func TestUploadIconRejectsExcessiveDimensions(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	const excessiveWidth = 2049
	icon := image.NewGray(image.Rect(0, 0, excessiveWidth, 1))
	if err := png.Encode(&body, icon); err != nil {
		t.Fatal(err)
	}
	if err := store.UploadIcon("example.com", 19132, body.Bytes(), "image/png"); err == nil {
		t.Fatalf("UploadIcon accepted a %d-pixel-wide icon", excessiveWidth)
	}
}

func TestUploadIconRejectsExcessivePixelCount(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	icon := image.NewGray(image.Rect(0, 0, 1025, 1024))
	if err := png.Encode(&body, icon); err != nil {
		t.Fatal(err)
	}
	if err := store.UploadIcon("example.com", 19132, body.Bytes(), "image/png"); err == nil {
		t.Fatal("UploadIcon accepted an icon with more than one megapixel")
	}
}

func TestAdapterServesValidatedPNGWithNosniff(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	icon := testPNG(t)
	if err := store.UploadIcon("example.com", 19132, icon, "image/png"); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/icons/example.com/19132/icon.png", nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET icon status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if !bytes.Equal(response.Body.Bytes(), icon) {
		t.Fatal("served icon differs from uploaded icon")
	}
}

func TestAdapterNeverServesActiveContentFromIconPath(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "icons", "example.com", "19132", "icon.png")
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("<script>alert(1)</script>"), 0o644); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/icons/example.com/19132/icon.png", nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET active icon status = %d, want 404", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got == "text/html; charset=utf-8" {
		t.Fatalf("active icon was labelled %q", got)
	}
}

func TestAdapterStoresJPEGAtCompatibleIconPathAndServesJPEG(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	icon := testJPEG(t)
	if err := store.UploadIcon("example.com", 19132, icon, "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "icons", "example.com", "19132", "icon.png")); err != nil {
		t.Fatalf("stat compatible icon.png path: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/icons/example.com/19132/icon.png", nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET icon status = %d, want 200", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", got)
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestAdapterPersistsDownloadStats(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload("example.com", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.com", 19132); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.com", 19132); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	stats := reloaded.Stats()
	if stats.SuccessfulDownloads != 2 || stats.UniqueSuccessfulTargets != 1 {
		t.Fatalf("Stats = %+v", stats)
	}
	servers := reloaded.Servers("")
	if len(servers) != 1 || servers[0].Host != "example.com" || servers[0].Downloads != 2 {
		t.Fatalf("Servers = %+v", servers)
	}
}

func TestRecordDownloadBatchesStatsPersistence(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload("example.com", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.com", 19132); err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().SuccessfulDownloads; got != 1 {
		t.Fatalf("in-memory downloads = %d, want 1", got)
	}

	body, err := os.ReadFile(filepath.Join(root, statsFilename))
	if err != nil {
		t.Fatal(err)
	}
	var persisted diskState
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if got := persisted.Targets[targetKey("example.com", 19132)].Downloads; got != 0 {
		t.Fatalf("synchronously persisted downloads = %d, want 0 before batch flush", got)
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordDownloadFlushesWithinBoundedDelay(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	store.flushDelay = 10 * time.Millisecond
	if err := store.Upload("example.com", 19132, bytes.NewReader([]byte("pack"))); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordDownload("example.com", 19132); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		body, readErr := os.ReadFile(filepath.Join(root, statsFilename))
		var persisted diskState
		if readErr == nil && json.Unmarshal(body, &persisted) == nil {
			if target := persisted.Targets[targetKey("example.com", 19132)]; target != nil && target.Downloads == 1 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("download stats were not flushed within 2 seconds")
}

func TestAdapterRejectsNonAssetPaths(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/", "/stats.json", "/packs/../../stats.json"} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		store.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			body, _ := io.ReadAll(response.Result().Body)
			t.Fatalf("GET %s = %d %q, want 404", target, response.Code, body)
		}
	}
}

func TestAdapterRejectsAssetSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	outsidePack := filepath.Join(outside, "secret.zip")
	if err := os.WriteFile(outsidePack, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "packs", "example.com", "19132", "example.com.zip")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePack, link); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/packs/example.com/19132/example.com.zip", nil)
	response := httptest.NewRecorder()
	store.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET escaping symlink status = %d, want 404", response.Code)
	}
}
