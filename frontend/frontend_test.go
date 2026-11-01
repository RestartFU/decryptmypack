package frontend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesAnonymousStandaloneFrontend(t *testing.T) {
	handler := Handler()
	for target, contentType := range map[string]string{
		"/":                            "text/html",
		"/style.css":                   "text/css",
		"/atlas.css":                   "text/css",
		"/src/script.js":               "text/javascript",
		"/favicon.ico":                 "image/x-icon",
		"/assets/favicon.svg":          "image/svg+xml",
		"/assets/apple-touch-icon.png": "image/png",
		"/assets/zeqa_icon.png":        "image/png",
		"/assets/Silkscreen.woff2":     "font/woff2",
		"/assets/Rubik.woff2":          "font/woff2",
		"/assets/JetBrainsMono.woff2":  "font/woff2",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", target, response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.Contains(got, contentType) {
			t.Fatalf("GET %s Content-Type = %q, want %q", target, got, contentType)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if policy := response.Header().Get("Content-Security-Policy"); !strings.Contains(policy, "default-src 'self'") || !strings.Contains(policy, "object-src 'none'") {
		t.Fatalf("Content-Security-Policy = %q", policy)
	}
	body := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{"sign in", "billing", "subscription", "username-modal", "config.js"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("index contains removed auth/billing term %q", forbidden)
		}
	}
}

func TestScriptUsesOnlyPublicSameOriginAPI(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/src/script.js", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("script status = %d", response.Code)
	}
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, required := range []string{"/download", "/status", "/stats", "/browse"} {
		if !strings.Contains(script, required) {
			t.Fatalf("script does not reference %s", required)
		}
	}
	for _, forbidden := range []string{"Authorization", "/auth/", "/billing/", "workers.dev", "currentUser"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("script contains removed dependency %q", forbidden)
		}
	}
}

func TestIndexPreservesOperationalDOMContract(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	index := response.Body.String()

	for _, id := range []string{
		"input", "decrypt-btn", "recent-targets",
		"status-card", "status-dot", "status-title", "status-text", "status-target",
		"status-actions", "status-wait-actions", "status-retry-actions",
		"download-again", "cancel-download-wait", "retry-download", "error",
		"total-downloads", "unique-downloads", "browse-search", "popular-server-list",
		"discord-btn", "discord-modal", "discord-dismiss", "discord-join", "status-stages",
	} {
		if count := strings.Count(index, `id="`+id+`"`); count != 1 {
			t.Fatalf("id %q occurs %d times, want exactly once", id, count)
		}
	}
}

func TestIndexHasStandaloneLandingPageStructure(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	index := response.Body.String()
	for _, required := range []string{
		`class="site-header"`,
		`class="hero"`,
		`id="how-it-works"`,
		`id="browse"`,
		`class="site-footer"`,
	} {
		if !strings.Contains(index, required) {
			t.Fatalf("index is missing landing-page structure %s", required)
		}
	}
}

func TestIndexIsCSPCompatibleAndKeepsProductPhrase(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	index := response.Body.String()

	if !strings.Contains(strings.ToLower(index), "decrypt my pack") {
		t.Fatal("index no longer contains the literal phrase \"decrypt my pack\"")
	}
	if count := strings.Count(index, "<script"); count != 1 || !strings.Contains(index, `<script src="/src/script.js?v=`) {
		t.Fatalf("index must contain exactly one external script tag, found %d script tags", count)
	}
	for _, forbidden := range []string{` style="`, `onclick=`, `onload=`, `src="http`, `href="http://`, `<link rel="stylesheet" href="http`, `@import`, `javascript:`} {
		if strings.Contains(index, forbidden) {
			t.Fatalf("index contains CSP-incompatible or remote markup %q", forbidden)
		}
	}
	for _, hook := range []string{`data-state="idle"`, `data-visible="false"`, `class="popular-empty"`, `class="hero"`} {
		if !strings.Contains(index, hook) {
			t.Fatalf("index is missing script hook %s", hook)
		}
	}
}

func TestIndexVersionsMutableStaticAssets(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	index := response.Body.String()
	for _, asset := range []string{`href="/atlas.css?v=`, `href="/style.css?v=`, `src="/src/script.js?v=`} {
		if !strings.Contains(index, asset) {
			t.Fatalf("index does not version mutable asset %s", asset)
		}
	}
}

func TestHandlerRevalidatesMutableStaticAssets(t *testing.T) {
	for _, target := range []string{"/atlas.css", "/style.css", "/src/script.js"} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("GET %s Cache-Control = %q, want no-cache", target, got)
		}
	}
}

func TestStylesheetUsesOnlyLocalResources(t *testing.T) {
	css := ""
	for _, target := range []string{"/atlas.css", "/style.css"} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		css += response.Body.String()
	}
	for _, forbidden := range []string{"http://", "https://", "@import"} {
		if strings.Contains(css, forbidden) {
			t.Fatalf("stylesheet references a remote resource via %q", forbidden)
		}
	}
	for _, required := range []string{
		`url("/assets/Silkscreen.woff2")`,
		`url("/assets/Silkscreen-Bold.woff2")`,
		`url("/assets/Rubik.woff2")`,
		`url("/assets/JetBrainsMono.woff2")`,
		"prefers-reduced-motion",
		`[data-state="loading"]`,
		`[data-visible="true"]`,
		`[data-stage-state="active"]`,
		".server-cell.is-selected",
		".cache-glyph-running",
		".recent-target",
	} {
		if !strings.Contains(css, required) {
			t.Fatalf("stylesheet is missing %s", required)
		}
	}
}

func TestScriptUsesCSPCompatibleStateAndSupportsRunningServers(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/src/script.js", nil))
	script := response.Body.String()
	if strings.Contains(script, ".style.") {
		t.Fatal("script mutates inline styles despite style-src 'self'")
	}
	if !strings.Contains(script, `["cached", "uncached", "running", "failed"]`) {
		t.Fatal("script does not render the backend running cache state")
	}
}

func TestRetiredAppPageRedirectsHome(t *testing.T) {
	for _, target := range []string{"/app", "/app/"} {
		response := httptest.NewRecorder()
		Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "/" {
			t.Fatalf("GET %s = %d %q, want a permanent redirect to /", target, response.Code, response.Header().Get("Location"))
		}
	}
}

func TestIndexDeclaresSiteIcons(t *testing.T) {
	response := httptest.NewRecorder()
	Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	index := response.Body.String()
	for _, link := range []string{`rel="icon" href="/favicon.ico"`, `rel="icon" href="/assets/favicon.svg"`, `rel="apple-touch-icon" href="/assets/apple-touch-icon.png"`} {
		if !strings.Contains(index, link) {
			t.Fatalf("index is missing icon link %s", link)
		}
	}
}
