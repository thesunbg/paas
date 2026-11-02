package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scan"
)

// handleScan scans an image for known vulnerabilities. The image comes from
// containerd (the build's output, or pulled for an image deployment the same
// way a worker would pull it) and is exported to a local archive: Trivy reads
// only that file and its cached DB, so it never talks to a tenant's registry
// and never runs anything from the image.
func (a *Agent) handleScan(ctx context.Context, req *proto.ScanRequest) *proto.ScanReply {
	if _, err := a.scanner.Path(); err != nil {
		return &proto.ScanReply{Missing: true, Error: err.Error()}
	}
	defer a.work()()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	lb := newLineBatcher(a.nc, req.LogSubject)
	defer lb.Close()
	res, err := a.scanImage(ctx, req, lb.Add)
	if err != nil {
		return &proto.ScanReply{Missing: errors.Is(err, scan.ErrMissing), Error: err.Error()}
	}
	return &proto.ScanReply{Result: res}
}

func (a *Agent) scanImage(ctx context.Context, req *proto.ScanRequest, log func(string)) (*proto.ScanResult, error) {
	if err := os.MkdirAll(a.builder.WorkDir, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(a.builder.WorkDir, "scan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "image.tar")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	log("==> scanning " + req.Image + " for known vulnerabilities")
	pulled, err := a.rt.ExportImage(ctx, req.Image, req.Pull, req.PlainHTTPRegistry, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if pulled && !slices.Contains(a.cfg.Roles, "worker") {
		// Pulled only to be scanned: a build-only node never runs it.
		defer a.rt.DropImage(context.WithoutCancel(ctx), req.Image)
	}
	if err != nil {
		return nil, err
	}
	return a.scanner.ScanArchive(ctx, path, log)
}
