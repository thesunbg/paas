package proxy

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/model"
)

func newTestProxy(routes ...model.Route) *Proxy {
	p := New(config.Proxy{}, nil)
	p.SetRoutes(&model.RouteTable{Version: 1, Routes: routes})
	return p
}

func get(t *testing.T, h http.Handler, host, path string, hdr ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func body(t *testing.T, r *http.Response) string {
	t.Helper()
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		rd = gz
	}
	b, _ := io.ReadAll(rd)
	return string(b)
}

func TestUpstreamRoundRobinAndRetry(t *testing.T) {
	var hits [2]int
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits[0]++; io.WriteString(w, "a:"+r.Host) }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits[1]++; io.WriteString(w, "b") }))
	defer b.Close()
	dead := "127.0.0.1:1" // nothing listens here
	p := newTestProxy(model.Route{Host: "app.test", ServiceID: "s1", Kind: "upstream",
		Upstreams: []string{strings.TrimPrefix(a.URL, "http://"), dead, strings.TrimPrefix(b.URL, "http://")}})
	for i := 0; i < 6; i++ {
		r := get(t, p, "app.test", "/")
		if r.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, r.StatusCode)
		}
	}
	if hits[0] == 0 || hits[1] == 0 {
		t.Fatalf("expected both upstreams used, got %v", hits)
	}
	if got := body(t, get(t, p, "APP.test:80", "/")); !strings.HasPrefix(got, "a:") && got != "b" {
		t.Fatalf("host lookup should be case-insensitive and ignore port: %q", got)
	}
}

func TestStaticSPA(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>"+strings.Repeat("x", 2000)+"</html>"), 0o644)
	os.MkdirAll(filepath.Join(root, "assets"), 0o755)
	os.WriteFile(filepath.Join(root, "assets", "app-3f9a8b7c6d.js"), []byte("console.log(1)"), 0o644)
	p := newTestProxy(model.Route{Host: "site.test", ServiceID: "s2", Kind: "static", StaticRoot: root, SPA: true})

	r := get(t, p, "site.test", "/dashboard/settings", "Accept-Encoding", "gzip")
	if r.StatusCode != 200 || !strings.Contains(body(t, r), "<html>") || r.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("spa fallback failed: %d %v", r.StatusCode, r.Header)
	}
	r = get(t, p, "site.test", "/assets/app-3f9a8b7c6d.js")
	if !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed asset should be immutable: %v", r.Header)
	}
	if r := get(t, p, "site.test", "/missing.png"); r.StatusCode != 404 {
		t.Fatalf("missing asset must 404, got %d", r.StatusCode)
	}
	if r := get(t, p, "site.test", "/../../etc/passwd"); r.StatusCode == 200 && strings.Contains(body(t, r), "root:") {
		t.Fatal("path traversal")
	}
}

func TestUnknownHostAndUnavailable(t *testing.T) {
	p := newTestProxy(model.Route{Host: "down.test", ServiceID: "s3", Kind: "unavailable"})
	if r := get(t, p, "nope.test", "/"); r.StatusCode != 404 {
		t.Fatalf("unknown host: %d", r.StatusCode)
	}
	if r := get(t, p, "down.test", "/"); r.StatusCode != 503 {
		t.Fatalf("unavailable: %d", r.StatusCode)
	}
}

func TestEdgeServerHeaderLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := edgeServer("", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	go srv.Serve(ln)
	defer srv.Close()
	for _, c := range []struct {
		size, want int
	}{{100 << 10, http.StatusRequestHeaderFieldsTooLarge}, {10 << 10, http.StatusOK}} {
		req, _ := http.NewRequest("GET", "http://"+ln.Addr().String(), nil)
		req.Header.Set("X-Big", strings.Repeat("a", c.size))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("header %d bytes: got %d, want %d", c.size, resp.StatusCode, c.want)
		}
	}
}
