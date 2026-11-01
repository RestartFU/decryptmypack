package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/restartfu/decryptmypack/internal/nettarget"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"golang.org/x/oauth2"
)

const (
	maxPackTransferHops        = 4
	packDialTimeout            = 3 * time.Minute
	targetResolutionTimeout    = 5 * time.Second
	resourcePackConnectTimeout = 30 * time.Second
	resourcePackRedirectLimit  = 10
)

var (
	proxies                 []proxyInfo
	logPackets              = os.Getenv("PACKET_LOG") == "1"
	minecraftFormattingCode = regexp.MustCompile(`§.`)
)

func init() {
	proxies, _ = loadProxies("proxies.txt")
}

type Adapter struct {
	tokenPath   string
	tokenSource oauth2.TokenSource
}

func New(tokenPath string) *Adapter {
	if strings.TrimSpace(tokenPath) == "" {
		tokenPath = "token.json"
	}
	return &Adapter{tokenPath: tokenPath}
}

// NewWithTokenSource creates a downloader scoped to one already authenticated account.
// The caller owns refreshing and persisting the source's credentials.
func NewWithTokenSource(source oauth2.TokenSource) *Adapter {
	return &Adapter{tokenSource: source}
}

func (a *Adapter) DownloadFrom(host, resolvedIP string, port int, progress func(string)) (io.Reader, error) {
	ctx, cancel := context.WithTimeout(context.Background(), packDialTimeout)
	defer cancel()
	return a.downloadFromContext(ctx, host, resolvedIP, port, progress)
}

// DownloadWithTokenSource downloads from an already resolved public address using one visitor's account
// instead of the service's own token.
func (a *Adapter) DownloadWithTokenSource(source oauth2.TokenSource, host, resolvedIP string, port int, progress func(string)) (io.Reader, error) {
	if source == nil {
		return nil, fmt.Errorf("minecraft auth token source is required")
	}
	return NewWithTokenSource(source).DownloadFrom(host, resolvedIP, port, progress)
}

// DownloadContext resolves and validates a public server before downloading its packs.
// It is intended for local clients that do not have the hosted service's resolver layer.
func (a *Adapter) DownloadContext(ctx context.Context, host string, port int, progress func(string)) (io.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, packDialTimeout)
	defer cancel()
	resolvedIP, err := resolvePublicPackTarget(ctx, host)
	if err != nil {
		return nil, err
	}
	return a.downloadFromContext(ctx, host, resolvedIP, port, progress)
}

func (a *Adapter) downloadFromContext(ctx context.Context, host, resolvedIP string, port int, progress func(string)) (io.Reader, error) {
	report := func(message string) {
		log.Printf("Download status for %s:%d: %s", host, port, message)
		if progress != nil {
			progress(message)
		}
	}
	log.Printf("Attempting download from %s via %s (%d)", host, resolvedIP, port)
	start := time.Now()
	report("connecting to server")

	var packs []*resource.Pack
	var err error
	if a.tokenSource != nil {
		packs, err = fetchPacksWithTokenSourceContext(ctx, host, resolvedIP, port, report, a.tokenSource)
	} else {
		packs, err = fetchPacksWithTokenContext(ctx, host, resolvedIP, port, report, a.tokenPath)
	}
	if err != nil {
		log.Printf("Error downloading pack from %s (%d): %s", host, port, err)
		return nil, err
	}
	log.Printf("Successfully downloaded pack from %s (%d) in %s", host, port, time.Since(start))
	if len(packs) == 0 {
		return nil, fmt.Errorf("server did not send any resource packs")
	}

	f := &bytes.Buffer{}
	zipFile := zip.NewWriter(f)
	seenNames := map[string]int{}
	for i, pack := range packs {
		report("encoding " + pack.Name())
		buf, err := EncodePack(pack)
		if err != nil {
			return nil, err
		}
		if len(buf) == 0 {
			return nil, fmt.Errorf("pack %q encoded to an empty archive", pack.Name())
		}

		if pack.Encrypted() {
			report("decrypting " + pack.Name())
			buf, err = DecryptPack(buf, pack.ContentKey())
			if err != nil {
				return nil, err
			}
		}
		if len(buf) == 0 {
			return nil, fmt.Errorf("pack %q decoded to an empty archive", pack.Name())
		}

		entry, err := zipFile.Create(packArchiveName(pack.Name(), i, seenNames))
		if err != nil {
			return nil, err
		}
		if _, err = entry.Write(buf); err != nil {
			return nil, err
		}
	}
	if err := zipFile.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(f.Bytes()), nil
}

func packArchiveName(name string, index int, seen map[string]int) string {
	base := sanitizePackName(name)
	if base == "" {
		base = fmt.Sprintf("pack-%d", index+1)
	}
	count := seen[base]
	seen[base] = count + 1
	if count > 0 {
		base = fmt.Sprintf("%s-%d", base, count+1)
	}
	return base + ".zip"
}

func sanitizePackName(name string) string {
	name = minecraftFormattingCode.ReplaceAllString(name, "")
	var b strings.Builder
	lastSpace := false
	writeSeparator := func() {
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|':
			writeSeparator()
		case r < 32 || r == 127 || unicode.IsSpace(r):
			writeSeparator()
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

type packTarget struct {
	host       string
	resolvedIP string
	port       int
}

type packResolver func(context.Context, string) (string, error)
type packDialer func(context.Context, packTarget, login.ClientData) ([]*resource.Pack, error)

func fetchPacks(host string, port int, progress func(string)) ([]*resource.Pack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), packDialTimeout)
	defer cancel()

	resolvedIP, err := resolvePublicPackTarget(ctx, host)
	if err != nil {
		return nil, err
	}
	return fetchPacksWithTokenContext(ctx, host, resolvedIP, port, progress, "token.json")
}

func fetchPacksWithToken(host, resolvedIP string, port int, progress func(string), tokenPath string) ([]*resource.Pack, error) {
	ctx, cancel := context.WithTimeout(context.Background(), packDialTimeout)
	defer cancel()
	return fetchPacksWithTokenContext(ctx, host, resolvedIP, port, progress, tokenPath)
}

func fetchPacksWithTokenContext(ctx context.Context, host, resolvedIP string, port int, progress func(string), tokenPath string) ([]*resource.Pack, error) {
	token, err := tokenSource(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("minecraft auth token source: %w", err)
	}
	return fetchPacksWithTokenSourceContext(ctx, host, resolvedIP, port, progress, token)
}

func fetchPacksWithTokenSourceContext(ctx context.Context, host, resolvedIP string, port int, progress func(string), token oauth2.TokenSource) ([]*resource.Pack, error) {
	if token == nil {
		return nil, fmt.Errorf("minecraft auth token source is required")
	}

	var network minecraft.Network = minecraft.RakNet{}
	if len(proxies) > 0 {
		network = NewAnonymousRakNet(proxies)
	}

	httpDialer := &net.Dialer{Timeout: resourcePackConnectTimeout, KeepAlive: 30 * time.Second}
	packHTTPClient := newResourcePackHTTPClient(net.DefaultResolver.LookupIPAddr, httpDialer.DialContext)
	profile := minecraft.DefaultClientData(net.JoinHostPort(host, strconv.Itoa(port)))

	dial := func(ctx context.Context, target packTarget, data login.ClientData) ([]*resource.Pack, error) {
		logicalAddress := net.JoinHostPort(target.host, strconv.Itoa(target.port))
		pinnedAddress := pinnedDialAddress(target.resolvedIP, target.port)
		conn, err := (minecraft.Dialer{
			ErrorLog:                   slog.Default(),
			HTTPClient:                 http.DefaultClient,
			TokenSource:                token,
			ClientData:                 data,
			ResourcePackHTTPClient:     packHTTPClient,
			DisconnectOnInvalidPackets: false,
			DisconnectOnUnknownPackets: false,
			PacketFunc: func(header packet.Header, _ []byte, src, dst net.Addr) {
				if logPackets {
					log.Printf("packet %s -> %s id=%d", src, dst, header.PacketID)
				}
			},
			DownloadResourcePack: func(_ uuid.UUID, _ string, current, total int) bool {
				if progress != nil {
					progress(fmt.Sprintf("downloading pack %d of %d", current+1, total))
				}
				return true
			},
		}).DialContextNetwork(ctx, pinnedNetwork{Network: network, address: pinnedAddress}, logicalAddress)
		if err != nil {
			return nil, err
		}
		packs := append([]*resource.Pack(nil), conn.ResourcePacks()...)
		if err := conn.Close(); err != nil {
			return nil, fmt.Errorf("close Minecraft connection: %w", err)
		}
		return packs, nil
	}

	return followPackTransfers(ctx, packTarget{host: host, resolvedIP: resolvedIP, port: port}, profile, resolvePublicPackTarget, dial, progress)
}

func followPackTransfers(ctx context.Context, initial packTarget, profile login.ClientData, resolve packResolver, dial packDialer, progress func(string)) ([]*resource.Pack, error) {
	if resolve == nil || dial == nil {
		return nil, fmt.Errorf("pack transfer resolver and dialer are required")
	}

	visited := make(map[string]struct{}, maxPackTransferHops+1)
	target := initial
	for hops := 0; ; hops++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target.host = nettarget.NormalizeHost(target.host)
		if err := validatePackTarget(target); err != nil {
			return nil, err
		}
		key := canonicalPackTarget(target.host, target.port)
		if _, exists := visited[key]; exists {
			return nil, fmt.Errorf("server transfer cycle detected at %s", key)
		}
		visited[key] = struct{}{}

		data := profile
		data.ServerAddress = net.JoinHostPort(target.host, strconv.Itoa(target.port))
		packs, err := dial(ctx, target, data)
		if err == nil {
			return packs, nil
		}

		var transfer *minecraft.TransferError
		if !errors.As(err, &transfer) {
			return nil, fmt.Errorf("dial Minecraft server %s: %w", key, err)
		}
		if hops >= maxPackTransferHops {
			return nil, fmt.Errorf("too many server transfers (maximum %d): %w", maxPackTransferHops, err)
		}

		nextHost := nettarget.NormalizeHost(transfer.Address)
		nextPort := int(transfer.Port)
		if nextHost == "" || nextPort < 1 {
			return nil, fmt.Errorf("server sent an invalid transfer target: %w", err)
		}
		nextKey := canonicalPackTarget(nextHost, nextPort)
		if _, exists := visited[nextKey]; exists {
			return nil, fmt.Errorf("server transfer cycle detected at %s: %w", nextKey, err)
		}
		if progress != nil {
			progress("following server transfer to " + nextKey)
		}
		resolvedIP, resolveErr := resolve(ctx, nextHost)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve transferred server %s: %w", nextHost, resolveErr)
		}
		target = packTarget{host: nextHost, resolvedIP: resolvedIP, port: nextPort}
	}
}

func validatePackTarget(target packTarget) error {
	if target.port < 1 || target.port > 65535 {
		return fmt.Errorf("invalid server port %d", target.port)
	}
	if ip := net.ParseIP(target.host); ip == nil {
		if !nettarget.ValidHostname(target.host) {
			return fmt.Errorf("invalid server hostname %q", target.host)
		}
	} else if !nettarget.PublicIP(ip) {
		return nettarget.ErrNonPublicTarget
	}
	if !nettarget.PublicIP(net.ParseIP(target.resolvedIP)) {
		return nettarget.ErrNonPublicTarget
	}
	return nil
}

func canonicalPackTarget(host string, port int) string {
	return net.JoinHostPort(nettarget.NormalizeHost(host), strconv.Itoa(port))
}

func resolvePublicPackTarget(ctx context.Context, host string) (string, error) {
	host = nettarget.NormalizeHost(host)
	if ip := net.ParseIP(host); ip != nil {
		if !nettarget.PublicIP(ip) {
			return "", nettarget.ErrNonPublicTarget
		}
		return ip.String(), nil
	}
	if !nettarget.ValidHostname(host) {
		return "", fmt.Errorf("invalid server hostname %q", host)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, targetResolutionTimeout)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return "", fmt.Errorf("resolve server hostname: %w", err)
	}
	public, err := nettarget.PublicIPStrings(addresses)
	if err != nil {
		return "", err
	}
	return public[0], nil
}

type pinnedNetwork struct {
	minecraft.Network
	address string
}

func (network pinnedNetwork) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	return network.Network.DialContext(ctx, network.address)
}

type lookupIPAddrFunc func(context.Context, string) ([]net.IPAddr, error)
type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func newResourcePackHTTPClient(resolve lookupIPAddrFunc, dial dialContextFunc) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse resource-pack address: %w", err)
		}

		var addresses []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			addresses = []net.IPAddr{{IP: ip}}
		} else {
			if !nettarget.ValidHostname(host) {
				return nil, fmt.Errorf("invalid resource-pack hostname %q", host)
			}
			addresses, err = resolve(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("resolve resource-pack hostname: %w", err)
			}
		}
		public, err := nettarget.PublicIPStrings(addresses)
		if err != nil {
			return nil, err
		}
		var dialErr error
		for _, ip := range public {
			connection, err := dial(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				return connection, nil
			}
			dialErr = errors.Join(dialErr, err)
		}
		return nil, dialErr
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= resourcePackRedirectLimit {
				return fmt.Errorf("too many resource-pack HTTP redirects")
			}
			if request.URL.Scheme != "http" && request.URL.Scheme != "https" {
				return fmt.Errorf("unsupported resource-pack URL scheme %q", request.URL.Scheme)
			}
			return nil
		},
	}
}

func pinnedDialAddress(ip string, port int) string {
	return net.JoinHostPort(ip, strconv.Itoa(port))
}
