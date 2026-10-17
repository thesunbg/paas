package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/proto"
)

func TestTransferMultipartPut(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = string(b)
		mu.Unlock()
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%s"`, strings.TrimPrefix(r.URL.Path, "/")))
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("aaaabbbbcc"), 0o600)
	a := &Agent{cfg: config.Agent{DataDir: dir}}

	rep := a.handleTransfer(context.Background(), &proto.TransferRequest{Method: http.MethodPut, Path: "f", PartSize: 4,
		PartURLs: []string{srv.URL + "/1", srv.URL + "/2", srv.URL + "/3"}})
	if rep.Error != "" || len(rep.ETags) != 3 || rep.ETags[0] != `"etag-1"` || rep.ETags[2] != `"etag-3"` {
		t.Fatalf("reply: %+v", rep)
	}
	if got["/1"] != "aaaa" || got["/2"] != "bbbb" || got["/3"] != "cc" {
		t.Fatalf("parts: %v", got)
	}

	// Too few parts for the file must not upload a truncated object.
	rep = a.handleTransfer(context.Background(), &proto.TransferRequest{Method: http.MethodPut, Path: "f", PartSize: 4, PartURLs: []string{srv.URL + "/9"}})
	if rep.Error == "" {
		t.Fatal("expected an error for parts that do not cover the file")
	}

	// A plain PUT still works and returns no ETags.
	rep = a.handleTransfer(context.Background(), &proto.TransferRequest{Method: http.MethodPut, Path: "f", URL: srv.URL + "/whole"})
	if rep.Error != "" || len(rep.ETags) != 0 || got["/whole"] != "aaaabbbbcc" {
		t.Fatalf("plain put: %+v %q", rep, got["/whole"])
	}
}
