package downloader

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"strings"

	"github.com/df-mc/jsonc"
	"github.com/sandertv/gophertunnel/minecraft/resource"
)

func EncodePack(pack *resource.Pack) ([]byte, error) {
	buf := make([]byte, pack.Len())
	off := 0
	for {
		n, err := pack.ReadAt(buf[off:], int64(off))
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		off += n
	}
	return buf, nil
}

func decryptCBF(data []byte, key []byte) ([]byte, error) {
	key = bytes.TrimSpace(key)
	switch len(key) {
	case 16, 24, 32:
	default:
		return nil, fmt.Errorf("invalid AES key length %d", len(key))
	}

	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	data = append([]byte(nil), data...)
	shiftRegister := append(append([]byte(nil), key[:16]...), data...) // prefill with iv + cipherdata
	tmp := make([]byte, 16)

	off := 0
	for off < len(data) {
		b.Encrypt(tmp, shiftRegister)
		data[off] ^= tmp[0]
		shiftRegister = shiftRegister[1:]
		off++
	}
	return data, nil
}

type ContentEntry struct {
	Path string `json:"path"`
	Key  string `json:"key"`
}

type Content struct {
	Content []ContentEntry `json:"content"`
}

func DecryptPack(buf []byte, key string) ([]byte, error) {
	rb := bytes.NewReader(buf)
	r, err := zip.NewReader(rb, rb.Size())
	if err != nil {
		return nil, err
	}
	zb := bytes.NewBuffer([]byte{})
	z := zip.NewWriter(zb)

	var content Content
	contentsFile, basePath, ok := findContentsJSON(r)
	if !ok {
		if hasManifestJSON(r) {
			return buf, nil
		}
		return nil, fmt.Errorf("contents.json not found")
	}

	cf, err := contentsFile.Open()
	if err != nil {
		return nil, fmt.Errorf("error opening contents.json: %w", err)
	}
	defer cf.Close()

	cbuf, _ := io.ReadAll(cf)
	decr, err := decryptContentsJSON(cbuf, []byte(key))
	if err != nil {
		return nil, err
	}

	cw, _ := z.Create("contents.json")
	_, _ = cw.Write(decr)
	if err = json.Unmarshal(decr, &content); err != nil {
		return nil, err
	}

	for _, entry := range content.Content {
		if isDirectoryPlaceholder(entry.Path, content.Content) {
			continue
		}
		filePath := entry.Path
		if len(basePath) > 0 {
			filePath = basePath + "/" + entry.Path
		}
		f, err := openZipPath(r, filePath)
		if err != nil {
			continue
		}
		fbuf, _ := io.ReadAll(f)
		_ = f.Close()
		if entry.Key != "" {
			decrypted, err := decryptCBF(fbuf, []byte(entry.Key))
			if err != nil {
				return nil, err
			}
			// Some packs list JSON files with a key even though raw bytes are plaintext.
			// Prefer plaintext if it parses and decrypted bytes do not.
			if isJSONPath(entry.Path) && !isJSONC(decrypted) && isJSONC(fbuf) {
				// Keep fbuf as-is.
			} else {
				fbuf = decrypted
			}
		}
		if isJSONPath(entry.Path) {
			fbuf = unobfuscateJSON(fbuf)
		}
		if isPNGPath(entry.Path) {
			fbuf = normalizePNG(fbuf)
		}
		fw, _ := z.Create(entry.Path)
		_, _ = fw.Write(fbuf)
	}

	_ = z.Close()
	return zb.Bytes(), nil
}

func decryptContentsJSON(rawContents, packKey []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(rawContents)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("contents.json is empty")
	}
	if json.Valid(trimmed) {
		return trimmed, nil
	}
	if len(rawContents) <= 0x100 {
		return nil, fmt.Errorf("encrypted contents.json is too small: %d bytes", len(rawContents))
	}
	decr, err := decryptCBF(rawContents[0x100:], packKey)
	if err != nil {
		return nil, err
	}
	decr = bytes.TrimSpace(bytes.Split(decr, []byte("\x00"))[0])
	if !json.Valid(decr) {
		return nil, fmt.Errorf("decrypted contents.json is not valid JSON")
	}
	return decr, nil
}

func unobfuscateJSON(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var v any
	if err := jsonc.Unmarshal(raw, &v); err != nil {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return raw
	}
	return buf.Bytes()
}

func isJSONC(b []byte) bool {
	if len(bytes.TrimSpace(b)) == 0 {
		return false
	}
	var v any
	return jsonc.Unmarshal(b, &v) == nil
}

func isJSONPath(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".json")
}

func isPNGPath(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".png")
}

func normalizePNG(raw []byte) []byte {
	if bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		return raw
	}
	img, err := decodeTGA(raw)
	if err != nil {
		return raw
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return raw
	}
	return buf.Bytes()
}

func decodeTGA(raw []byte) (image.Image, error) {
	if len(raw) < 18 {
		return nil, fmt.Errorf("tga too small")
	}

	idLength := int(raw[0])
	colorMapType := raw[1]
	imageType := raw[2]
	width := int(raw[12]) | int(raw[13])<<8
	height := int(raw[14]) | int(raw[15])<<8
	bitsPerPixel := int(raw[16])
	descriptor := raw[17]

	if colorMapType != 0 || (imageType != 2 && imageType != 3) {
		return nil, fmt.Errorf("unsupported tga type")
	}
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid tga size")
	}
	if imageType == 2 && bitsPerPixel != 24 && bitsPerPixel != 32 {
		return nil, fmt.Errorf("unsupported tga bpp")
	}
	if imageType == 3 && bitsPerPixel != 8 {
		return nil, fmt.Errorf("unsupported tga bpp")
	}

	bytesPerPixel := bitsPerPixel / 8
	offset := 18 + idLength
	required := offset + width*height*bytesPerPixel
	if len(raw) < required {
		return nil, fmt.Errorf("truncated tga")
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	topOrigin := descriptor&0x20 != 0
	rightOrigin := descriptor&0x10 != 0

	for y := 0; y < height; y++ {
		dstY := y
		if !topOrigin {
			dstY = height - 1 - y
		}
		for x := 0; x < width; x++ {
			dstX := x
			if rightOrigin {
				dstX = width - 1 - x
			}
			i := offset + (y*width+x)*bytesPerPixel
			if imageType == 3 {
				v := raw[i]
				img.SetNRGBA(dstX, dstY, color.NRGBA{
					R: v,
					G: v,
					B: v,
					A: 0xff,
				})
				continue
			}
			a := uint8(0xff)
			if bytesPerPixel == 4 {
				a = raw[i+3]
			}
			img.SetNRGBA(dstX, dstY, color.NRGBA{
				R: raw[i+2],
				G: raw[i+1],
				B: raw[i],
				A: a,
			})
		}
	}

	return img, nil
}

func isDirectoryPlaceholder(entryPath string, entries []ContentEntry) bool {
	entryPath = strings.Trim(strings.ReplaceAll(entryPath, "\\", "/"), "/")
	if entryPath == "" {
		return true
	}
	prefix := entryPath + "/"
	for _, entry := range entries {
		otherPath := strings.Trim(strings.ReplaceAll(entry.Path, "\\", "/"), "/")
		if strings.HasPrefix(otherPath, prefix) {
			return true
		}
	}
	return false
}

func findContentsJSON(r *zip.Reader) (*zip.File, string, bool) {
	var fallback *zip.File
	for _, f := range r.File {
		name := strings.Trim(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if !strings.EqualFold(pathBase(name), "contents.json") {
			continue
		}
		if strings.EqualFold(name, "contents.json") {
			return f, "", true
		}
		if fallback == nil {
			fallback = f
		}
	}
	if fallback == nil {
		return nil, "", false
	}
	name := strings.Trim(strings.ReplaceAll(fallback.Name, "\\", "/"), "/")
	return fallback, pathDir(name), true
}

func hasManifestJSON(r *zip.Reader) bool {
	for _, f := range r.File {
		name := strings.Trim(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if strings.EqualFold(pathBase(name), "manifest.json") {
			return true
		}
	}
	return false
}

func openZipPath(r *zip.Reader, name string) (io.ReadCloser, error) {
	if f, err := r.Open(name); err == nil {
		return f, nil
	}
	wanted := strings.Trim(strings.ReplaceAll(name, "\\", "/"), "/")
	for _, f := range r.File {
		candidate := strings.Trim(strings.ReplaceAll(f.Name, "\\", "/"), "/")
		if strings.EqualFold(candidate, wanted) {
			return f.Open()
		}
	}
	return nil, os.ErrNotExist
}

func pathBase(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func pathDir(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}
