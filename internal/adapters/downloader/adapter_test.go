package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/resource"
)

func TestPinnedDialAddressSupportsIPv4AndIPv6(t *testing.T) {
	for _, test := range []struct {
		host string
		want string
	}{
		{host: "8.8.8.8", want: "8.8.8.8:19132"},
		{host: "2606:4700:4700::1111", want: "[2606:4700:4700::1111]:19132"},
	} {
		if got := pinnedDialAddress(test.host, 19132); got != test.want {
			t.Fatalf("pinnedDialAddress(%q) = %q, want %q", test.host, got, test.want)
		}
	}
}

func TestFollowPackTransfersPinsAddressesAndPreservesProfile(t *testing.T) {
	initial := packTarget{host: "zeqa.net", resolvedIP: "40.223.14.30", port: 19132}
	profile := login.ClientData{DeviceID: "stable-device", ClientRandomID: 42}
	var attempts []packTarget

	packs, err := followPackTransfers(t.Context(), initial, profile,
		func(_ context.Context, host string) (string, error) {
			if host != "pvp.inpvp.net" {
				t.Fatalf("resolver host = %q", host)
			}
			return "172.65.200.198", nil
		},
		func(_ context.Context, target packTarget, data login.ClientData) ([]*resource.Pack, error) {
			attempts = append(attempts, target)
			if data.DeviceID != profile.DeviceID || data.ClientRandomID != profile.ClientRandomID {
				t.Fatalf("client identity changed across transfer: %#v", data)
			}
			if data.ServerAddress != net.JoinHostPort(target.host, strconv.Itoa(target.port)) {
				t.Fatalf("ServerAddress = %q, want logical target", data.ServerAddress)
			}
			if len(attempts) == 1 {
				return nil, fmt.Errorf("dial entry: %w", &minecraft.TransferError{Address: "pvp.inpvp.net", Port: 19132})
			}
			return []*resource.Pack{nil}, nil
		}, nil)
	if err != nil {
		t.Fatalf("followPackTransfers(): %v", err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs = %d, want 1", len(packs))
	}
	want := []packTarget{
		{host: "zeqa.net", resolvedIP: "40.223.14.30", port: 19132},
		{host: "pvp.inpvp.net", resolvedIP: "172.65.200.198", port: 19132},
	}
	if fmt.Sprint(attempts) != fmt.Sprint(want) {
		t.Fatalf("attempts = %#v, want %#v", attempts, want)
	}
}

func TestFollowPackTransfersRejectsCycles(t *testing.T) {
	attempts := 0
	_, err := followPackTransfers(t.Context(), packTarget{host: "zeqa.net", resolvedIP: "40.223.14.30", port: 19132}, login.ClientData{},
		func(context.Context, string) (string, error) {
			t.Fatal("cycle should be rejected before DNS resolution")
			return "", nil
		},
		func(context.Context, packTarget, login.ClientData) ([]*resource.Pack, error) {
			attempts++
			return nil, &minecraft.TransferError{Address: "ZEQA.NET.", Port: 19132}
		}, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Fatalf("followPackTransfers() error = %v, want cycle error", err)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
}

func TestFollowPackTransfersRejectsUnsafeRedirect(t *testing.T) {
	unsafe := errors.New("target must resolve only to public addresses")
	attempts := 0
	_, err := followPackTransfers(t.Context(), packTarget{host: "zeqa.net", resolvedIP: "40.223.14.30", port: 19132}, login.ClientData{},
		func(context.Context, string) (string, error) { return "", unsafe },
		func(context.Context, packTarget, login.ClientData) ([]*resource.Pack, error) {
			attempts++
			return nil, &minecraft.TransferError{Address: "127.0.0.1", Port: 19132}
		}, nil)
	if !errors.Is(err, unsafe) {
		t.Fatalf("followPackTransfers() error = %v, want %v", err, unsafe)
	}
	if attempts != 1 {
		t.Fatalf("dial attempts = %d, want 1", attempts)
	}
}

func TestFollowPackTransfersBoundsRedirects(t *testing.T) {
	attempts := 0
	_, err := followPackTransfers(t.Context(), packTarget{host: "entry.example", resolvedIP: "8.8.8.8", port: 19132}, login.ClientData{},
		func(_ context.Context, host string) (string, error) { return "1.1.1.1", nil },
		func(context.Context, packTarget, login.ClientData) ([]*resource.Pack, error) {
			attempts++
			return nil, &minecraft.TransferError{Address: fmt.Sprintf("hop-%d.example", attempts), Port: 19132}
		}, nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "too many") {
		t.Fatalf("followPackTransfers() error = %v, want hop-limit error", err)
	}
	if attempts != maxPackTransferHops+1 {
		t.Fatalf("dial attempts = %d, want %d", attempts, maxPackTransferHops+1)
	}
}

func TestResourcePackHTTPClientPinsPublicResolution(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	port := strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port)
	dialed := make(chan string, 1)
	client := newResourcePackHTTPClient(
		func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "packs.example" {
				t.Fatalf("resolver host = %q", host)
			}
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		},
		func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed <- address
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		},
	)
	response, err := client.Get("http://packs.example:" + port + "/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got, want := <-dialed, net.JoinHostPort("8.8.8.8", port); got != want {
		t.Fatalf("dialed = %q, want pinned %q", got, want)
	}
}

func TestResourcePackHTTPClientRejectsPrivateResolution(t *testing.T) {
	client := newResourcePackHTTPClient(
		func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
		func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("private target was dialed")
			return nil, nil
		},
	)
	if _, err := client.Get("https://packs.example/pack.zip"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "public") {
		t.Fatalf("GET error = %v, want public-address rejection", err)
	}
}

func TestResourcePackHTTPClientUsesCallerDeadline(t *testing.T) {
	client := newResourcePackHTTPClient(
		func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("unused") },
		func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") },
	)
	if client.Timeout != 0 {
		t.Fatalf("client timeout = %s, want caller context to own the overall deadline", client.Timeout)
	}
}
