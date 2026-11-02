package runtime

import (
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/errdefs"
)

// ExportImage writes ref (this machine's platform only) as an OCI archive to w,
// pulling it first when pull is set and the node does not have it. pulled reports
// whether this call fetched it, so the caller can drop it again (DropImage).
func (r *Runtime) ExportImage(ctx context.Context, ref string, pull bool, plainHost string, w io.Writer) (pulled bool, err error) {
	cctx := r.ctx(ctx)
	_, err = r.images.GetImage(cctx, ref)
	switch {
	case err == nil:
	case errdefs.IsNotFound(err) && pull:
		if _, err := r.pull(cctx, ref, plainHost); err != nil {
			return false, fmt.Errorf("pull %s: %w", ref, err)
		}
		pulled = true
	default:
		return false, fmt.Errorf("image %s: %w", ref, err)
	}
	img, err := r.client.GetImage(cctx, ref)
	if err != nil {
		return pulled, fmt.Errorf("image %s: %w", ref, err)
	}
	err = r.client.Export(cctx, w,
		archive.WithImage(r.client.ImageService(), ref),
		archive.WithPlatform(img.Platform()),
		// Attestations and other platforms' blobs of an index are not on the node.
		archive.WithSkipMissing(r.client.ContentStore()),
	)
	if err != nil {
		return pulled, fmt.Errorf("export %s: %w", ref, err)
	}
	return pulled, nil
}

// DropImage removes an image no container on this node uses.
func (r *Runtime) DropImage(ctx context.Context, ref string) {
	r.mu.Lock()
	for _, m := range r.containers {
		if m.Req.Image == ref {
			r.mu.Unlock()
			return
		}
	}
	r.mu.Unlock()
	_ = r.client.ImageService().Delete(r.ctx(ctx), ref, images.SynchronousDelete())
}
