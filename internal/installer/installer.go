// Package installer embeds the node installation script served at /install.sh.
package installer

import (
	_ "embed"
	"net/http"
)

//go:embed install.sh
var script []byte

// Handler serves the install script.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(script)
}
