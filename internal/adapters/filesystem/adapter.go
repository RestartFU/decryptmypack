package filesystem

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/restartfu/decryptmypack/internal/ports"
)

const (
	statsFilename    = "stats.json"
	maxIconBytes     = 2 << 20
	maxIconDimension = 2048
	maxIconPixels    = 1 << 20
	statsFlushDelay  = 5 * time.Second
)

type Adapter struct {
	root string

	mu               sync.RWMutex
	state            diskState
	version          uint64
	persistedVersion uint64
	flushTimer       *time.Timer
	flushMu          sync.Mutex
	flushDelay       time.Duration
}

type diskState struct {
	Targets map[string]*targetStats `json:"targets"`
}

type targetStats struct {
	Host             string `json:"host"`
	Port             int    `json:"port"`
	Downloads        int64  `json:"downloads"`
	LastDownloadedAt string `json:"last_downloaded_at,omitempty"`
}

func New(root string) (*Adapter, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("data directory is empty")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory: %w", err)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve data directory symlinks: %w", err)
	}

	a := &Adapter{
		root: root,
		state: diskState{
			Targets: make(map[string]*targetStats),
		},
		flushDelay: statsFlushDelay,
	}
	if err := a.loadStats(); err != nil {
		return nil, err
	}
	if err := a.discoverPacks(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Adapter) Upload(host string, port int, packs io.Reader) error {
	host, err := normaliseTarget(host, port)
	if err != nil {
		return err
	}
	if err := writeReaderAtomic(a.packPath(host, port), packs, 0o644); err != nil {
		return err
	}

	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	a.mu.Lock()
	a.ensureTargetLocked(host, port)
	a.version++
	version := a.version
	snapshot := cloneDiskState(a.state)
	if a.flushTimer != nil {
		a.flushTimer.Stop()
		a.flushTimer = nil
	}
	a.mu.Unlock()

	err = a.persist(snapshot)
	a.mu.Lock()
	if err == nil && a.persistedVersion < version {
		a.persistedVersion = version
	}
	if a.persistedVersion < a.version {
		a.scheduleFlushLocked()
	}
	a.mu.Unlock()
	return err
}

func (a *Adapter) UploadIcon(host string, port int, icon []byte, contentType string) error {
	if len(icon) == 0 {
		return nil
	}
	host, err := normaliseTarget(host, port)
	if err != nil {
		return err
	}
	if _, err := validateIcon(icon, contentType); err != nil {
		return err
	}
	return writeReaderAtomic(a.iconPath(host, port), bytes.NewReader(icon), 0o644)
}

func validateIcon(icon []byte, contentType string) (string, error) {
	if len(icon) > maxIconBytes {
		return "", fmt.Errorf("icon exceeds %d byte limit", maxIconBytes)
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return "", fmt.Errorf("invalid icon content type: %w", err)
	}
	switch mediaType {
	case "image/png":
		config, err := png.DecodeConfig(bytes.NewReader(icon))
		if err != nil {
			return "", fmt.Errorf("invalid PNG icon header: %w", err)
		}
		if err := validateIconDimensions(config.Width, config.Height); err != nil {
			return "", err
		}
		if _, err := png.Decode(bytes.NewReader(icon)); err != nil {
			return "", fmt.Errorf("invalid PNG icon: %w", err)
		}
		return "image/png", nil
	case "image/jpeg":
		config, err := jpeg.DecodeConfig(bytes.NewReader(icon))
		if err != nil {
			return "", fmt.Errorf("invalid JPEG icon header: %w", err)
		}
		if err := validateIconDimensions(config.Width, config.Height); err != nil {
			return "", err
		}
		if _, err := jpeg.Decode(bytes.NewReader(icon)); err != nil {
			return "", fmt.Errorf("invalid JPEG icon: %w", err)
		}
		return "image/jpeg", nil
	default:
		return "", fmt.Errorf("unsupported icon content type %q", mediaType)
	}
}

func validateIconDimensions(width, height int) error {
	if width < 1 || height < 1 || width > maxIconDimension || height > maxIconDimension {
		return fmt.Errorf("icon dimensions %dx%d exceed %d pixel limit", width, height, maxIconDimension)
	}
	if int64(width)*int64(height) > maxIconPixels {
		return fmt.Errorf("icon dimensions %dx%d exceed %d pixel area limit", width, height, maxIconPixels)
	}
	return nil
}

func (a *Adapter) FetchDownloadURL(host string, port int) (string, error) {
	host, err := normaliseTarget(host, port)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(a.packPath(host, port))
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	return path.Join("/packs", url.PathEscape(host), strconv.Itoa(port), url.PathEscape(host)+".zip"), nil
}

func (a *Adapter) RecordDownload(host string, port int) error {
	host, err := normaliseTarget(host, port)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	target := a.ensureTargetLocked(host, port)
	target.Downloads++
	target.LastDownloadedAt = time.Now().UTC().Format(time.RFC3339)
	a.version++
	a.scheduleFlushLocked()
	return nil
}

// Flush persists any batched download-stat updates. It is primarily useful for
// graceful shutdowns and deterministic tests; normal operation flushes on a
// bounded background timer.
func (a *Adapter) Flush() error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	a.mu.Lock()
	if a.flushTimer != nil {
		a.flushTimer.Stop()
		a.flushTimer = nil
	}
	if a.persistedVersion >= a.version {
		a.mu.Unlock()
		return nil
	}
	version := a.version
	snapshot := cloneDiskState(a.state)
	a.mu.Unlock()

	err := a.persist(snapshot)
	a.mu.Lock()
	if err == nil && a.persistedVersion < version {
		a.persistedVersion = version
	}
	if a.persistedVersion < a.version {
		a.scheduleFlushLocked()
	}
	a.mu.Unlock()
	return err
}

func (a *Adapter) scheduleFlushLocked() {
	if a.flushTimer != nil {
		return
	}
	a.flushTimer = time.AfterFunc(a.flushDelay, func() {
		if err := a.Flush(); err != nil {
			log.Printf("could not persist download stats: %v", err)
		}
	})
}

func (a *Adapter) Stats() ports.DownloadStats {
	a.mu.RLock()
	defer a.mu.RUnlock()
	stats := ports.DownloadStats{UniqueSuccessfulTargets: len(a.state.Targets)}
	for _, target := range a.state.Targets {
		stats.SuccessfulDownloads += target.Downloads
	}
	return stats
}

func (a *Adapter) Servers(query string) []ports.Server {
	query = strings.ToLower(strings.TrimSpace(query))
	a.mu.RLock()
	servers := make([]ports.Server, 0, len(a.state.Targets))
	for _, target := range a.state.Targets {
		formatted := target.Host
		if target.Port != 19132 {
			formatted += ":" + strconv.Itoa(target.Port)
		}
		if query != "" && !strings.Contains(strings.ToLower(formatted), query) {
			continue
		}
		servers = append(servers, ports.Server{
			Host:             target.Host,
			Port:             target.Port,
			Downloads:        target.Downloads,
			LastDownloadedAt: target.LastDownloadedAt,
		})
	}
	a.mu.RUnlock()

	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Downloads != servers[j].Downloads {
			return servers[i].Downloads > servers[j].Downloads
		}
		if servers[i].LastDownloadedAt != servers[j].LastDownloadedAt {
			return servers[i].LastDownloadedAt > servers[j].LastDownloadedAt
		}
		if servers[i].Host != servers[j].Host {
			return servers[i].Host < servers[j].Host
		}
		return servers[i].Port < servers[j].Port
	})
	return servers
}

func (a *Adapter) Handler() http.Handler {
	return http.HandlerFunc(a.serveAsset)
}

func (a *Adapter) serveAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	clean := path.Clean("/" + strings.TrimLeft(r.URL.Path, "/"))
	if !strings.HasPrefix(clean, "/packs/") && !strings.HasPrefix(clean, "/icons/") {
		http.NotFound(w, r)
		return
	}
	relative := strings.TrimPrefix(clean, "/")
	filename := filepath.Join(a.root, filepath.FromSlash(relative))
	filename, err := filepath.EvalSymlinks(filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	relativeToRoot, err := filepath.Rel(a.root, filename)
	if err != nil || filepath.IsAbs(relativeToRoot) || relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}

	file, err := os.Open(filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	if strings.HasPrefix(clean, "/packs/") {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(clean)))
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
		return
	}

	icon, err := io.ReadAll(io.LimitReader(file, maxIconBytes+1))
	if err != nil || len(icon) > maxIconBytes {
		http.NotFound(w, r)
		return
	}
	contentType, err := safeStoredIconContentType(icon)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", path.Base(clean)))
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, info.Name(), info.ModTime(), bytes.NewReader(icon))
}

func safeStoredIconContentType(icon []byte) (string, error) {
	if contentType, err := validateIcon(icon, "image/png"); err == nil {
		return contentType, nil
	}
	return validateIcon(icon, "image/jpeg")
}

func (a *Adapter) loadStats() error {
	body, err := os.ReadFile(filepath.Join(a.root, statsFilename))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, &a.state); err != nil {
		return fmt.Errorf("read local stats: %w", err)
	}
	if a.state.Targets == nil {
		a.state.Targets = make(map[string]*targetStats)
	}
	return nil
}

func (a *Adapter) discoverPacks() error {
	pattern := filepath.Join(a.root, "packs", "*", "*", "*.zip")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	changed := false
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, filename := range files {
		relative, err := filepath.Rel(filepath.Join(a.root, "packs"), filename)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 3 {
			continue
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil || strings.TrimSuffix(parts[2], ".zip") != parts[0] {
			continue
		}
		key := targetKey(parts[0], port)
		if _, exists := a.state.Targets[key]; exists {
			continue
		}
		a.ensureTargetLocked(parts[0], port)
		changed = true
	}
	if changed {
		return a.persistLocked()
	}
	return nil
}

func (a *Adapter) ensureTargetLocked(host string, port int) *targetStats {
	key := targetKey(host, port)
	if target := a.state.Targets[key]; target != nil {
		return target
	}
	target := &targetStats{Host: host, Port: port}
	a.state.Targets[key] = target
	return target
}

func (a *Adapter) persistLocked() error {
	return a.persist(a.state)
}

func (a *Adapter) persist(state diskState) error {
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeReaderAtomic(filepath.Join(a.root, statsFilename), bytes.NewReader(body), 0o644)
}

func cloneDiskState(state diskState) diskState {
	clone := diskState{Targets: make(map[string]*targetStats, len(state.Targets))}
	for key, target := range state.Targets {
		copied := *target
		clone.Targets[key] = &copied
	}
	return clone
}

func (a *Adapter) packPath(host string, port int) string {
	return filepath.Join(a.root, "packs", host, strconv.Itoa(port), host+".zip")
}

func (a *Adapter) iconPath(host string, port int) string {
	return filepath.Join(a.root, "icons", host, strconv.Itoa(port), "icon.png")
}

func normaliseTarget(host string, port int) (string, error) {
	host = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	if host == "" || strings.ContainsAny(host, `/\\`) || host == "." || host == ".." {
		return "", fmt.Errorf("invalid host")
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port")
	}
	return host, nil
}

func targetKey(host string, port int) string {
	return strings.ToLower(host) + ":" + strconv.Itoa(port)
}

func writeReaderAtomic(filename string, reader io.Reader, mode os.FileMode) (err error) {
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err = io.Copy(temporary, reader); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filename)
}
