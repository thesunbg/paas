package proxy

import (
	"compress/gzip"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var hashedAsset = regexp.MustCompile(`[.-][A-Za-z0-9_-]{8,}\.(js|mjs|css|woff2?|png|jpe?g|gif|svg|webp|avif|ico|map)$`)

var gzipPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed); return w }}

func compressible(contentType string) bool {
	for _, p := range []string{"text/", "application/javascript", "application/json", "image/svg+xml", "application/xml", "application/wasm", "application/manifest+json"} {
		if strings.HasPrefix(contentType, p) {
			return true
		}
	}
	return false
}

// serveStatic serves a file from root with SPA fallback, caching and compression.
func serveStatic(w http.ResponseWriter, r *http.Request, root string, spa bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		errorPage(w, http.StatusMethodNotAllowed, "Method not allowed", "")
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	full := filepath.Join(root, filepath.FromSlash(clean))
	if !strings.HasPrefix(full, filepath.Clean(root)) {
		errorPage(w, http.StatusBadRequest, "Bad request", "")
		return
	}
	status := http.StatusOK
	st, err := os.Stat(full)
	if err == nil && st.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		full = filepath.Join(full, "index.html")
		st, err = os.Stat(full)
	}
	if err != nil {
		// Pretty URLs: /about -> /about.html
		if alt := full + ".html"; fileExists(alt) {
			full = alt
		} else if nf := filepath.Join(root, "404.html"); !spa && fileExists(nf) {
			full, status = nf, http.StatusNotFound
		} else if spa && path.Ext(clean) == "" {
			full = filepath.Join(root, "index.html")
		} else {
			errorPage(w, http.StatusNotFound, "Page not found", "")
			return
		}
	}
	f, err := os.Open(full)
	if err != nil {
		errorPage(w, http.StatusNotFound, "Page not found", "")
		return
	}
	defer f.Close()
	st, err = f.Stat()
	if err != nil {
		errorPage(w, http.StatusInternalServerError, "Cannot read file", "")
		return
	}
	ctype := mime.TypeByExtension(filepath.Ext(full))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	switch {
	case hashedAsset.MatchString(full) || strings.Contains(clean, "/assets/") || strings.Contains(clean, "/_next/static/"):
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasPrefix(ctype, "text/html"):
		h.Set("Cache-Control", "public, max-age=0, must-revalidate")
	default:
		h.Set("Cache-Control", "public, max-age=3600")
	}
	accept := r.Header.Get("Accept-Encoding")
	h.Add("Vary", "Accept-Encoding")
	// Precompressed siblings produced by the build (file.br / file.gz).
	for _, enc := range []struct{ name, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if strings.Contains(accept, enc.name) {
			if pf, err := os.Open(full + enc.ext); err == nil {
				defer pf.Close()
				h.Set("Content-Encoding", enc.name)
				w.WriteHeader(status)
				if r.Method != http.MethodHead {
					_, _ = io.Copy(w, pf)
				}
				return
			}
		}
	}
	if status == http.StatusOK && (!compressible(ctype) || st.Size() < 1024 || !strings.Contains(accept, "gzip")) {
		http.ServeContent(w, r, full, st.ModTime(), f)
		return
	}
	if compressible(ctype) && st.Size() >= 1024 && strings.Contains(accept, "gzip") {
		h.Set("Content-Encoding", "gzip")
		h.Del("Content-Length")
		w.WriteHeader(status)
		if r.Method == http.MethodHead {
			return
		}
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(w)
		_, _ = io.Copy(gz, f)
		_ = gz.Close()
		gzipPool.Put(gz)
		return
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func errorPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%d %s</title>
<style>body{font-family:system-ui,sans-serif;background:#0b0d12;color:#e6e8ee;display:grid;place-items:center;min-height:100vh;margin:0}
main{max-width:32rem;padding:2rem}h1{font-size:1.4rem;margin:0 0 .5rem}p{color:#9aa3b2;line-height:1.5}code{color:#7aa2ff}</style></head>
<body><main><p><code>%d</code></p><h1>%s</h1><p>%s</p></main></body></html>`, status, html.EscapeString(title), status, html.EscapeString(title), html.EscapeString(detail))
}
