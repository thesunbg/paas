// Package installer embeds the node installation script served at /install.sh.
package installer

import (
	_ "embed"
	"net/http"
	"os"
	"runtime"
	"strings"
)

//go:embed install.sh
var script []byte

// Handler serves the install script.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(script)
}

// BinaryHandler serves this server's own paasd binary to joining nodes of the
// same architecture: GET /download/paasd-linux-<arch>.
func BinaryHandler(w http.ResponseWriter, r *http.Request) {
	arch, ok := strings.CutPrefix(r.PathValue("file"), "paasd-linux-")
	if !ok {
		http.NotFound(w, r)
		return
	}
	if arch == "x86_64" {
		arch = "amd64"
	}
	if arch == "aarch64" {
		arch = "arm64"
	}
	if arch != runtime.GOARCH {
		http.Error(w, "this server only has a linux-"+runtime.GOARCH+" binary", http.StatusNotFound)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, exe)
}
