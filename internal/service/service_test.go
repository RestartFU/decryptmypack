package service

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/restartfu/decryptmypack/internal/domain"
)

func TestHTTPServerHasPublicSafeReadLimits(t *testing.T) {
	server := newHTTPServer(":0", http.NotFoundHandler())
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v", server.ReadHeaderTimeout)
	}
	if server.ReadTimeout != 15*time.Second {
		t.Fatalf("ReadTimeout = %v", server.ReadTimeout)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout = %v", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes = %d", server.MaxHeaderBytes)
	}
}

func TestExtractQueryPortValidatesRange(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Port
		err  error
	}{
		{name: "default", raw: "", want: 19132},
		{name: "valid", raw: "19133", want: 19133},
		{name: "lowest", raw: "1", want: 1},
		{name: "highest", raw: "65535", want: 65535},
		{name: "zero", raw: "0", err: domain.ErrPortNotValidNumber},
		{name: "negative", raw: "-1", err: domain.ErrPortNotValidNumber},
		{name: "too high", raw: "65536", err: domain.ErrPortNotValidNumber},
		{name: "not numeric", raw: "abc", err: domain.ErrPortNotValidNumber},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := url.Values{}
			if tt.raw != "" {
				values.Set("port", tt.raw)
			}

			got, err := extractQueryPort(values)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("error = %v, want %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("port = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractQueryIPRejectsMalformedHostsBeforeDNS(t *testing.T) {
	tests := []string{
		"",
		"bad_host.example",
		"-bad.example",
		"bad-.example",
		"foo:bar",
		"example..com",
	}

	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			values := url.Values{}
			values.Set("host", raw)

			if _, err := extractQueryIP(values); !errors.Is(err, domain.ErrHostInvalid) {
				t.Fatalf("extractQueryIP(%q) error = %v, want %v", raw, err, domain.ErrHostInvalid)
			}
		})
	}
}

func TestExtractQueryIPNormalizesIPLiteral(t *testing.T) {
	got, err := extractQueryIP(url.Values{"host": []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "127.0.0.1" {
		t.Fatalf("ip = %q, want 127.0.0.1", got)
	}
}

func TestPublicTargetIPRejectsInternalNetworks(t *testing.T) {
	tests := []struct {
		address string
		want    bool
	}{
		{address: "8.8.8.8", want: true},
		{address: "2606:4700:4700::1111", want: true},
		{address: "127.0.0.1", want: false},
		{address: "10.0.0.1", want: false},
		{address: "100.64.0.1", want: false},
		{address: "169.254.1.1", want: false},
		{address: "192.0.2.1", want: false},
		{address: "198.18.0.1", want: false},
		{address: "198.51.100.1", want: false},
		{address: "203.0.113.1", want: false},
		{address: "224.0.0.1", want: false},
		{address: "::1", want: false},
		{address: "fc00::1", want: false},
		{address: "fe80::1", want: false},
		{address: "2001:db8::1", want: false},
		{address: "64:ff9b::7f00:1", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			if got := publicTargetIP(tt.address); got != tt.want {
				t.Fatalf("publicTargetIP(%q) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}

func TestSelectPublicTargetIPPinsOnePublicResolution(t *testing.T) {
	got, err := selectPublicTargetIP([]net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("1.1.1.1")})
	if err != nil {
		t.Fatal(err)
	}
	if got != "8.8.8.8" {
		t.Fatalf("selected IP = %q, want 8.8.8.8", got)
	}
}

func TestSelectPublicTargetIPRejectsMixedPublicAndPrivateResolution(t *testing.T) {
	if _, err := selectPublicTargetIP([]net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("127.0.0.1")}); err == nil {
		t.Fatal("mixed public/private resolution was accepted")
	}
}

func TestReadAllLimitedRejectsOversizedPack(t *testing.T) {
	_, err := readAllLimited(io.LimitReader(zeroReader{}, 9), 8)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want too-large error", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestValidatePackArchiveRejectsAndroidIncompatibleOuterNames(t *testing.T) {
	archive := testOuterArchive(t, map[string][]byte{
		"Hive: Murder Mystery.zip": []byte("pack"),
	})

	err := validatePackArchive(archive)
	if err == nil {
		t.Fatal("expected Android-incompatible filename to be rejected")
	}
	if !strings.Contains(err.Error(), "Android-incompatible filename") {
		t.Fatalf("error = %v, want Android-incompatible filename", err)
	}
}

func TestValidatePackArchiveAcceptsPortableOuterNames(t *testing.T) {
	archive := testOuterArchive(t, map[string][]byte{
		"Hive Murder Mystery.zip": []byte("pack"),
	})

	if err := validatePackArchive(archive); err != nil {
		t.Fatalf("validatePackArchive returned error: %v", err)
	}
}

func testOuterArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
