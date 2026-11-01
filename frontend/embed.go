package frontend

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed static/*.html static/*.css src/script.js assets/*
var files embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(serve)
}

func serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	filename := ""
	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		filename = "static/index.html"
		w.Header().Set("Cache-Control", "no-cache")
	case r.URL.Path == "/app" || r.URL.Path == "/app/":
		// The desktop app was retired; old links land on the website tool.
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
		return
	case r.URL.Path == "/style.css" || r.URL.Path == "/atlas.css":
		filename = "static" + r.URL.Path
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case r.URL.Path == "/src/script.js":
		filename = "src/script.js"
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	case r.URL.Path == "/favicon.ico":
		filename = "assets/favicon.ico"
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		filename = strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	default:
		http.NotFound(w, r)
		return
	}
	body, err := fs.ReadFile(files, filename)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if w.Header().Get("Content-Type") == "" {
		extension := strings.ToLower(path.Ext(filename))
		contentType := mime.TypeByExtension(extension)
		switch extension {
		case ".otf":
			contentType = "font/otf"
		case ".woff2":
			contentType = "font/woff2"
		case ".ico":
			contentType = "image/x-icon"
		case ".svg":
			contentType = "image/svg+xml"
		}
		if contentType == "" {
			contentType = http.DetectContentType(body)
		}
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if strings.HasPrefix(filename, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
