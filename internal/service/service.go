package service

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/restartfu/decryptmypack/frontend"
	"github.com/restartfu/decryptmypack/internal/domain"
	"github.com/restartfu/decryptmypack/internal/nettarget"
	"github.com/restartfu/decryptmypack/internal/ports"
	"github.com/restartfu/decryptmypack/internal/servers"
	"github.com/restartfu/decryptmypack/internal/webaccounts"
	"golang.org/x/oauth2"
)

type (
	IP   string
	Port int
)

const (
	defaultMaxConcurrentDownloads = 2
	maxPackArchiveBytes           = int64(512 << 20)
	maxPackIconBytes              = int64(5 << 20)
	targetResolutionTimeout       = 5 * time.Second
)

type Service struct {
	store       ports.Store
	downloader  ports.Downloader
	webAccounts *webaccounts.Store
	// serviceAccount reports whether the service has its own Microsoft token for visitors without an account.
	serviceAccount bool

	mu         sync.Mutex
	processing map[IP][]Port
	failures   map[IP]map[Port]error
	statuses   map[IP]map[Port]string
	slots      chan struct{}
	resolve    func(string) (string, error)
}

type Option func(*Service)

func WithMaxConcurrentDownloads(maximum int) Option {
	return func(s *Service) {
		if maximum > 0 {
			s.slots = make(chan struct{}, maximum)
		}
	}
}

func withTargetResolver(resolver func(string) (string, error)) Option {
	return func(s *Service) {
		if resolver != nil {
			s.resolve = resolver
		}
	}
}

func New(store ports.Store, downloader ports.Downloader, opts ...Option) *Service {
	s := &Service{
		store:      store,
		downloader: downloader,
		processing: make(map[IP][]Port),
		failures:   make(map[IP]map[Port]error),
		statuses:   make(map[IP]map[Port]string),
		slots:      make(chan struct{}, defaultMaxConcurrentDownloads),
		resolve:    resolvePublicTarget,

		serviceAccount: true,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) Start(addr string) error {
	return s.StartContext(context.Background(), addr)
}

func (s *Service) StartContext(ctx context.Context, addr string) error {
	if ctx.Err() != nil {
		return s.store.Flush()
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()

	server := newHTTPServer(addr, s.Handler())
	serveDone := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			shutdownErr := server.Shutdown(shutdownCtx)
			_ = listener.Close()
			shutdownResult <- shutdownErr
		case <-serveDone:
			shutdownResult <- nil
		}
	}()

	serveErr := server.Serve(listener)
	close(serveDone)
	shutdownErr := <-shutdownResult
	if errors.Is(serveErr, http.ErrServerClosed) || (ctx.Err() != nil && errors.Is(serveErr, net.ErrClosed)) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr, s.store.Flush())
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.Health)
	mux.HandleFunc("/download", s.Download)
	mux.HandleFunc("/status", s.Status)
	mux.HandleFunc("/stats", s.Stats)
	mux.HandleFunc("/browse", s.Browse)
	mux.HandleFunc("/file", s.File)
	s.registerAccountRoutes(mux)
	mux.Handle("/packs/", s.store.Handler())
	mux.Handle("/icons/", s.store.Handler())
	mux.Handle("/", frontend.Handler())
	return mux
}

func (s *Service) Health(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "decryptmypack",
		"storage": "local",
	})
}

func (s *Service) Stats(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.store.Stats())
}

type browseResponse struct {
	Featured []browseServer `json:"featured"`
	Popular  []browseServer `json:"popular"`
}

type browseServer struct {
	Host             string `json:"host"`
	Port             string `json:"port"`
	Target           string `json:"target"`
	Name             string `json:"name,omitempty"`
	Icon             string `json:"icon,omitempty"`
	Downloads        int64  `json:"downloads"`
	LastDownloadedAt string `json:"last_downloaded_at,omitempty"`
	CacheState       string `json:"cache_state"`
}

func (s *Service) Browse(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	featured := servers.Featured()
	result := browseResponse{
		Featured: make([]browseServer, 0, len(featured)),
		Popular:  make([]browseServer, 0),
	}
	featuredTargets := make(map[string]struct{}, len(featured))
	downloads := make(map[string]int64)
	for _, server := range s.store.Servers("") {
		downloads[targetKey(server.Host, server.Port)] = server.Downloads
	}
	for _, template := range featured {
		server := browseServer{Host: template.Host, Port: template.Port, Target: template.Target, Name: template.Name, Icon: template.Icon}
		featuredTargets[targetKey(server.Host, parseBrowsePort(server.Port))] = struct{}{}
		server.Downloads = downloads[targetKey(server.Host, parseBrowsePort(server.Port))]
		server.CacheState = s.cacheState(server.Host, parseBrowsePort(server.Port))
		if query == "" || strings.Contains(strings.ToLower(server.Name+" "+server.Target), query) {
			result.Featured = append(result.Featured, server)
		}
	}

	for _, stored := range s.store.Servers(query) {
		if _, ok := featuredTargets[targetKey(stored.Host, stored.Port)]; ok {
			continue
		}
		target := stored.Host
		if stored.Port != 19132 {
			target += ":" + strconv.Itoa(stored.Port)
		}
		result.Popular = append(result.Popular, browseServer{
			Host:             stored.Host,
			Port:             strconv.Itoa(stored.Port),
			Target:           target,
			Icon:             path.Join("/icons", stored.Host, strconv.Itoa(stored.Port), "icon.png"),
			Downloads:        stored.Downloads,
			LastDownloadedAt: stored.LastDownloadedAt,
			CacheState:       s.cacheState(stored.Host, stored.Port),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Service) cacheState(host string, port int) string {
	if _, err := s.store.FetchDownloadURL(host, port); err == nil {
		return "cached"
	}
	ip := IP(host)
	p := Port(port)
	if s.isProcessing(ip, p) {
		return "running"
	}
	if s.failure(ip, p) != nil {
		return "failed"
	}
	return "uncached"
}

func (s *Service) Download(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	ip, err := extractQueryIP(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	port, err := extractQueryPort(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if downloadURL, err := s.store.FetchDownloadURL(string(ip), int(port)); err == nil {
		if err := s.store.RecordDownload(string(ip), int(port)); err != nil {
			log.Printf("could not update local download stats for %s:%d: %v", ip, port, err)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, downloadURL)
		return
	}

	if err := s.popFailure(ip, port); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tokenSource, err := s.downloadTokenSource(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if tokenSource == nil && !s.serviceAccount {
		http.Error(w, errAccountRequired.Error(), http.StatusUnauthorized)
		return
	}

	switch s.tryStart(ip, port) {
	case startAlreadyRunning:
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "processing")
		return
	case startBusy:
		http.Error(w, "download service is busy; try again shortly", http.StatusServiceUnavailable)
		return
	}

	resolvedIP, err := s.resolve(string(ip))
	if err != nil {
		s.finishProcessing(ip, port)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.setStatus(ip, port, "queued")
	go s.download(ip, port, resolvedIP, tokenSource)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "processing")
}

// download fetches one target in the background, with the visitor's account when one was chosen,
// and stores the result in the shared cache either way.
func (s *Service) download(ip IP, port Port, resolvedIP string, tokenSource oauth2.TokenSource) {
	defer s.finishProcessing(ip, port)
	report := func(message string) {
		s.setStatus(ip, port, message)
	}
	var reader io.Reader
	var err error
	if tokenSource != nil {
		accountDownloader, ok := s.downloader.(ports.AccountDownloader)
		if !ok {
			s.fail(ip, port, errors.New("this server cannot download with your own account"))
			return
		}
		reader, err = accountDownloader.DownloadWithTokenSource(tokenSource, string(ip), resolvedIP, int(port), report)
	} else {
		reader, err = s.downloader.DownloadFrom(string(ip), resolvedIP, int(port), report)
	}
	if err != nil {
		s.fail(ip, port, err)
		return
	}
	packBytes, err := readAllLimited(reader, maxPackArchiveBytes)
	if err != nil {
		s.fail(ip, port, err)
		return
	}
	if err := validatePackArchive(packBytes); err != nil {
		s.fail(ip, port, err)
		return
	}

	if logo, contentType := extractPackLogo(packBytes); len(logo) > 0 {
		s.setStatus(ip, port, "saving server icon")
		if err := s.store.UploadIcon(string(ip), int(port), logo, contentType); err != nil {
			log.Printf("could not save pack icon for %s:%d: %v", ip, port, err)
		}
	}
	s.setStatus(ip, port, "saving pack")
	if err := s.store.Upload(string(ip), int(port), bytes.NewReader(packBytes)); err != nil {
		s.fail(ip, port, fmt.Errorf("%w: %s", domain.ErrCouldNotUploadPack, err))
		return
	}
	s.clearFailure(ip, port)
	s.clearStatus(ip, port)
}

func (s *Service) Status(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	ip, err := extractQueryIP(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	port, err := extractQueryPort(r.URL.Query())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	if failure := s.failure(ip, port); failure != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "failed", "error": failure.Error()})
		return
	}
	status := s.status(ip, port)
	if status == "" {
		if _, err := s.store.FetchDownloadURL(string(ip), int(port)); err == nil {
			status = "ready"
		} else {
			status = "idle"
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Service) File(w http.ResponseWriter, r *http.Request) {
	if !requireGet(w, r) {
		return
	}
	ip, err := extractQueryIP(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	port, err := extractQueryPort(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	downloadURL, err := s.store.FetchDownloadURL(string(ip), int(port))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, downloadURL, http.StatusTemporaryRedirect)
}

func validatePackArchive(packBytes []byte) error {
	if len(packBytes) == 0 {
		return fmt.Errorf("downloaded pack archive is empty")
	}
	outer, err := zip.NewReader(bytes.NewReader(packBytes), int64(len(packBytes)))
	if err != nil {
		return fmt.Errorf("downloaded pack archive is invalid: %w", err)
	}
	if len(outer.File) == 0 {
		return fmt.Errorf("downloaded pack archive has no packs")
	}
	for _, f := range outer.File {
		if f.UncompressedSize64 > uint64(maxPackArchiveBytes) {
			return fmt.Errorf("downloaded pack archive member %q is too large", f.Name)
		}
		if !portableArchiveMemberName(f.Name) {
			return fmt.Errorf("downloaded pack archive contains Android-incompatible filename %q", f.Name)
		}
		if strings.HasSuffix(strings.ToLower(f.Name), ".zip") && f.UncompressedSize64 > 0 {
			return nil
		}
	}
	return fmt.Errorf("downloaded pack archive has no non-empty pack zip")
}

func portableArchiveMemberName(name string) bool {
	if name == "" || name != path.Base(name) {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return false
		}
	}
	return true
}

func extractPackLogo(packArchive []byte) ([]byte, string) {
	logo, contentType := extractLogoFromZip(packArchive)
	if len(logo) > 0 {
		return logo, contentType
	}
	outer, err := zip.NewReader(bytes.NewReader(packArchive), int64(len(packArchive)))
	if err != nil {
		return nil, ""
	}
	for _, f := range outer.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".zip") {
			continue
		}
		r, err := f.Open()
		if err != nil {
			continue
		}
		data, err := readAllLimited(r, maxPackArchiveBytes)
		_ = r.Close()
		if err != nil {
			continue
		}
		logo, contentType = extractLogoFromZip(data)
		if len(logo) > 0 {
			return logo, contentType
		}
	}
	return nil, ""
}

func extractLogoFromZip(data []byte) ([]byte, string) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, ""
	}
	for _, f := range zr.File {
		name := strings.ToLower(path.Base(f.Name))
		if name != "pack_icon.png" && name != "pack_icon.jpg" && name != "pack_icon.jpeg" {
			continue
		}
		r, err := f.Open()
		if err != nil {
			continue
		}
		logo, err := readAllLimited(r, maxPackIconBytes)
		_ = r.Close()
		if err != nil || len(logo) == 0 {
			continue
		}
		if strings.HasSuffix(name, ".jpg") || strings.HasSuffix(name, ".jpeg") {
			return logo, "image/jpeg"
		}
		return logo, "image/png"
	}
	return nil, ""
}

type startState int

const (
	startNew startState = iota
	startAlreadyRunning
	startBusy
)

func (s *Service) tryStart(ip IP, port Port) startState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.processing[ip], port) {
		return startAlreadyRunning
	}
	select {
	case s.slots <- struct{}{}:
		s.deleteFailureLocked(ip, port)
		s.processing[ip] = append(s.processing[ip], port)
		return startNew
	default:
		return startBusy
	}
}

func (s *Service) isProcessing(ip IP, port Port) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.processing[ip], port)
}

func (s *Service) finishProcessing(ip IP, port Port) {
	s.mu.Lock()
	ports := s.processing[ip]
	for i, processingPort := range ports {
		if processingPort == port {
			ports = slices.Delete(ports, i, i+1)
			break
		}
	}
	if len(ports) == 0 {
		delete(s.processing, ip)
	} else {
		s.processing[ip] = ports
	}
	s.mu.Unlock()
	<-s.slots
}

func (s *Service) setStatus(ip IP, port Port, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses[ip] == nil {
		s.statuses[ip] = make(map[Port]string)
	}
	s.statuses[ip][port] = status
}

func (s *Service) status(ip IP, port Port) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[ip][port]
}

func (s *Service) clearStatus(ip IP, port Port) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statuses[ip] == nil {
		return
	}
	delete(s.statuses[ip], port)
	if len(s.statuses[ip]) == 0 {
		delete(s.statuses, ip)
	}
}

func (s *Service) fail(ip IP, port Port, err error) {
	s.markFailure(ip, port, err)
	s.setStatus(ip, port, "failed")
}

func (s *Service) markFailure(ip IP, port Port, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures[ip] == nil {
		s.failures[ip] = make(map[Port]error)
	}
	s.failures[ip][port] = err
}

func (s *Service) failure(ip IP, port Port) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures[ip][port]
}

func (s *Service) popFailure(ip IP, port Port) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.failures[ip][port]
	if err != nil {
		s.deleteFailureLocked(ip, port)
		if s.statuses[ip] != nil {
			delete(s.statuses[ip], port)
		}
	}
	return err
}

func (s *Service) clearFailure(ip IP, port Port) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteFailureLocked(ip, port)
}

func (s *Service) deleteFailureLocked(ip IP, port Port) {
	if s.failures[ip] == nil {
		return
	}
	delete(s.failures[ip], port)
	if len(s.failures[ip]) == 0 {
		delete(s.failures, ip)
	}
}

func extractQueryIP(v url.Values) (IP, error) {
	host, _ := splitTarget(v)
	if ip := net.ParseIP(host); ip != nil {
		return IP(ip.String()), nil
	}
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimSuffix(host, ".")
	if strings.Contains(host, "hivebedrock.network") {
		host = "geo.hivebedrock.network"
	}
	if !isValidHostname(host) {
		return "", domain.ErrHostInvalid
	}
	return IP(host), nil
}

func resolvePublicTarget(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return selectPublicTargetIP([]net.IP{ip})
	}
	ctx, cancel := context.WithTimeout(context.Background(), targetResolutionTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", domain.ErrHostInvalid
	}
	return selectPublicTargetIP(ips)
}

func selectPublicTargetIP(ips []net.IP) (string, error) {
	selected, err := nettarget.SelectPublicIP(ips)
	if errors.Is(err, nettarget.ErrNoAddresses) {
		return "", domain.ErrHostIpListEmpty
	}
	if err != nil {
		return "", err
	}
	return selected, nil
}

func isValidHostname(host string) bool {
	return nettarget.ValidHostname(host)
}

func publicTargetIP(address string) bool {
	return nettarget.PublicIP(net.ParseIP(address))
}

func readAllLimited(reader io.Reader, maximum int64) ([]byte, error) {
	if maximum < 1 {
		return nil, fmt.Errorf("invalid read limit")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maximum {
		return nil, fmt.Errorf("downloaded pack is too large (maximum %d bytes)", maximum)
	}
	return body, nil
}

func extractQueryPort(v url.Values) (Port, error) {
	_, port := splitTarget(v)
	if port == "" {
		return Port(19132), nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, domain.ErrPortNotValidNumber
	}
	return Port(n), nil
}

func splitTarget(v url.Values) (string, string) {
	host := strings.TrimSpace(v.Get("host"))
	port := strings.TrimSpace(v.Get("port"))
	if host != "" {
		return strings.Trim(host, "[]"), port
	}
	target := strings.TrimSpace(v.Get("target"))
	if target == "" {
		return "", port
	}
	if strings.Contains(target, "://") {
		if parsed, err := url.Parse(target); err == nil {
			if external := parsed.Query().Get("addExternalServer"); external != "" {
				if decoded, err := url.QueryUnescape(external); err == nil {
					external = decoded
				}
				parts := strings.Split(external, "|")
				target = strings.TrimSpace(parts[len(parts)-1])
			} else if parsed.Hostname() != "" {
				if port == "" {
					port = parsed.Port()
				}
				return parsed.Hostname(), port
			}
		}
	}
	if targetHost, targetPort, err := net.SplitHostPort(target); err == nil {
		if port == "" {
			port = strings.TrimSpace(targetPort)
		}
		return strings.Trim(targetHost, "[]"), port
	}
	if strings.Count(target, ":") == 1 {
		targetHost, targetPort, _ := strings.Cut(target, ":")
		if port == "" {
			port = strings.TrimSpace(targetPort)
		}
		return strings.TrimSpace(targetHost), port
	}
	return strings.Trim(target, "[]"), port
}

func requireGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("could not encode JSON response: %v", err)
	}
}

func targetKey(host string, port int) string {
	return strings.ToLower(host) + ":" + strconv.Itoa(port)
}

func parseBrowsePort(raw string) int {
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 19132
	}
	return port
}
