package downloader

import (
	"archive/zip"
	"bytes"
	"image/png"
	"io"
	"testing"
)

func TestUnobfuscateJSON(t *testing.T) {
	raw := []byte(`{"name":"\u005a\u0065\u0071\u0061","items":[1,2]}`)

	got := unobfuscateJSON(raw)

	if !bytes.Contains(got, []byte(`"name": "Zeqa"`)) {
		t.Fatalf("expected unicode escapes to be decoded, got %s", got)
	}
	if !bytes.Contains(got, []byte(`"items": [`)) {
		t.Fatalf("expected json to be formatted, got %s", got)
	}
}

func TestDecryptPackPlaintextContentsAndJSONFallback(t *testing.T) {
	key := "1234567890123456"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeZipFile(t, zw, "contents.json", []byte(`{"content":[{"path":"textures","key":""},{"path":"manifest.json","key":"1234567890123456"},{"path":"textures/item.png","key":""}]}`))
	writeZipFile(t, zw, "manifest.json", []byte(`{"header":{"name":"\u005a\u0065\u0071\u0061"}}`))
	writeZipFile(t, zw, "textures/item.png", testTGA())
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	decrypted, err := DecryptPack(buf.Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(decrypted), int64(len(decrypted)))
	if err != nil {
		t.Fatal(err)
	}

	manifest := readZipPath(t, zr, "manifest.json")
	if !bytes.Contains(manifest, []byte(`"name": "Zeqa"`)) {
		t.Fatalf("expected plaintext JSON fallback plus deobfuscation, got %s", manifest)
	}

	gotPNG := readZipPath(t, zr, "textures/item.png")
	if !bytes.HasPrefix(gotPNG, []byte("\x89PNG")) {
		t.Fatalf("expected tga bytes to normalize to png, got %x", gotPNG[:8])
	}
	if _, err := png.Decode(bytes.NewReader(gotPNG)); err != nil {
		t.Fatalf("normalized png does not decode: %v", err)
	}
	for _, f := range zr.File {
		if f.Name == "textures" {
			t.Fatal("directory placeholder was written as a file")
		}
	}
}

func TestDecryptPackFindsCaseInsensitiveContentsName(t *testing.T) {
	key := "1234567890123456"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeZipFile(t, zw, "Contents.JSON", []byte(`{"content":[{"path":"Manifest.JSON","key":""}]}`))
	writeZipFile(t, zw, "Manifest.JSON", []byte(`{"header":{"name":"\u0053\u0077\u0069\u006e\u0067\u0067"}}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	decrypted, err := DecryptPack(buf.Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(decrypted), int64(len(decrypted)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := readZipPath(t, zr, "Manifest.JSON")
	if !bytes.Contains(manifest, []byte(`"name": "Swingg"`)) {
		t.Fatalf("expected case-insensitive contents lookup and JSON cleanup, got %s", manifest)
	}
}

func TestDecryptPackFindsNestedContentsBasePath(t *testing.T) {
	key := "1234567890123456"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeZipFile(t, zw, "packs/swingg/contents.json", []byte(`{"content":[{"path":"manifest.json","key":""}]}`))
	writeZipFile(t, zw, "packs/swingg/manifest.json", []byte(`{"header":{"name":"Nested"}}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	decrypted, err := DecryptPack(buf.Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(decrypted), int64(len(decrypted)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := readZipPath(t, zr, "manifest.json")
	if !bytes.Contains(manifest, []byte(`"name": "Nested"`)) {
		t.Fatalf("expected nested contents base path to be used, got %s", manifest)
	}
}

func TestDecryptPackAllowsPlainPackWithoutContentsJSON(t *testing.T) {
	key := "1234567890123456"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	writeZipFile(t, zw, "manifest.json", []byte(`{"header":{"name":"Plain keyed pack"}}`))
	writeZipFile(t, zw, "textures/item.png", []byte("plain image bytes"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	decrypted, err := DecryptPack(buf.Bytes(), key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, buf.Bytes()) {
		t.Fatal("expected plaintext pack without contents.json to pass through unchanged")
	}

	zr, err := zip.NewReader(bytes.NewReader(decrypted), int64(len(decrypted)))
	if err != nil {
		t.Fatal(err)
	}
	manifest := readZipPath(t, zr, "manifest.json")
	if !bytes.Contains(manifest, []byte("Plain keyed pack")) {
		t.Fatalf("expected manifest to remain readable, got %s", manifest)
	}
}

func TestNormalizePNGConvertsMonoTGA(t *testing.T) {
	got := normalizePNG(testMonoTGA())
	if !bytes.HasPrefix(got, []byte("\x89PNG")) {
		t.Fatalf("expected mono tga to convert to png, got %x", got)
	}
	if _, err := png.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("normalized mono png does not decode: %v", err)
	}
}

func TestPackArchiveNameSanitizesMinecraftFormatting(t *testing.T) {
	seen := map[string]int{}
	got := packArchiveName("§b§lEnch§r§d§lanted§r\n§fHub Pack§r", 0, seen)
	if got != "Enchanted Hub Pack.zip" {
		t.Fatalf("unexpected sanitized pack name: %q", got)
	}

	got = packArchiveName("§b§lEnch§r§d§lanted§r\n§fHub Pack§r", 1, seen)
	if got != "Enchanted Hub Pack-2.zip" {
		t.Fatalf("unexpected duplicate pack name: %q", got)
	}
}

func TestPackArchiveNameSanitizesAndroidIncompatibleCharacters(t *testing.T) {
	seen := map[string]int{}
	got := packArchiveName(`Hive: Murder Mystery/Blocks*?"<>|`, 0, seen)
	if got != "Hive Murder Mystery Blocks.zip" {
		t.Fatalf("unexpected Android-safe pack name: %q", got)
	}
}

func testTGA() []byte {
	header := []byte{
		0, 0, 2, // no ID, no color map, uncompressed true-color
		0, 0, 0, 0, 0,
		0, 0, 0, 0,
		1, 0, // width
		1, 0, // height
		32, // bpp
		8,  // alpha bits, bottom-left origin
	}
	// BGRA red pixel.
	return append(header, 0, 0, 255, 255)
}

func testMonoTGA() []byte {
	header := []byte{
		0, 0, 3, // no ID, no color map, uncompressed grayscale
		0, 0, 0, 0, 0,
		0, 0, 0, 0,
		1, 0, // width
		1, 0, // height
		8, // bpp
		0,
	}
	return append(header, 128)
}

func writeZipFile(t *testing.T, zw *zip.Writer, name string, data []byte) {
	t.Helper()
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
}

func readZipPath(t *testing.T, zr *zip.Reader, name string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	t.Fatalf("missing %s", name)
	return nil
}
