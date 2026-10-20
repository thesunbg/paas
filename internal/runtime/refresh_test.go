package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeImage is an image whose index lives in a real (temporary) content store.
type fakeImage struct {
	client.Image
	target ocispec.Descriptor
	store  content.Store
	plat   platforms.MatchComparer
}

func (f *fakeImage) Name() string                      { return "fake" }
func (f *fakeImage) Target() ocispec.Descriptor        { return f.target }
func (f *fakeImage) ContentStore() content.Store       { return f.store }
func (f *fakeImage) Platform() platforms.MatchComparer { return f.plat }

// fakeImages is a node's image store: ref -> image, counting pulls.
type fakeImages struct {
	have    map[string]client.Image
	pulled  map[string]client.Image // what a pull of ref delivers
	pullErr error
	pulls   int
}

func (f *fakeImages) GetImage(_ context.Context, ref string) (client.Image, error) {
	if img, ok := f.have[ref]; ok {
		return img, nil
	}
	return nil, errdefs.ErrNotFound
}

func (f *fakeImages) Pull(_ context.Context, ref string, _ ...client.RemoteOpt) (client.Image, error) {
	f.pulls++
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	f.have[ref] = f.pulled[ref]
	return f.pulled[ref], nil
}

// indexImage writes a multi-arch index (amd64, arm64 and an attestation entry) and returns an
// image for arm64 plus the digests of the index and of the arm64 manifest.
func indexImage(t *testing.T, salt string) (img *fakeImage, index, arm64 digest.Digest) {
	t.Helper()
	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(mt string, v any) ocispec.Descriptor {
		raw, _ := json.Marshal(v)
		d := ocispec.Descriptor{MediaType: mt, Digest: digest.FromBytes(raw), Size: int64(len(raw))}
		if err := content.WriteBlob(context.Background(), store, d.Digest.String(), bytes.NewReader(raw), d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	mf := func(arch string) ocispec.Descriptor {
		d := put(ocispec.MediaTypeImageManifest, map[string]string{"arch": arch, "salt": salt})
		d.Platform = &ocispec.Platform{OS: "linux", Architecture: arch}
		return d
	}
	amd, arm := mf("amd64"), mf("arm64")
	att := put(ocispec.MediaTypeImageManifest, map[string]string{"attestation": salt})
	att.Platform = &ocispec.Platform{OS: "unknown", Architecture: "unknown"}
	idx := put(ocispec.MediaTypeImageIndex, ocispec.Index{Manifests: []ocispec.Descriptor{att, amd, arm}})
	return &fakeImage{target: idx, store: store, plat: platforms.Only(ocispec.Platform{OS: "linux", Architecture: "arm64"})}, idx.Digest, arm.Digest
}

func TestRefreshImagePullsDespiteLocalImageAndReportsPlatformDigest(t *testing.T) {
	oldImg, _, oldArm := indexImage(t, "old")
	newImg, newIndex, newArm := indexImage(t, "new")
	fi := &fakeImages{have: map[string]client.Image{"postgres:17": oldImg}, pulled: map[string]client.Image{"postgres:17": newImg}}
	r := &Runtime{images: fi, ns: "t"}

	// EnsureImage keeps a local tag as it is: that is the freeze RefreshImage exists to break.
	if _, err := r.EnsureImage(context.Background(), "postgres:17", true, ""); err != nil || fi.pulls != 0 {
		t.Fatalf("EnsureImage pulled %d times, err %v", fi.pulls, err)
	}
	got, err := r.RefreshImage(context.Background(), "postgres:17", "")
	if err != nil {
		t.Fatal(err)
	}
	if fi.pulls != 1 {
		t.Fatalf("pulls = %d, want 1 (image was already local)", fi.pulls)
	}
	if got != newArm.String() || got == newIndex.String() || got == oldArm.String() {
		t.Fatalf("digest = %s, want the new arm64 manifest %s (index %s)", got, newArm, newIndex)
	}
}

func TestRefreshImagePullFailure(t *testing.T) {
	oldImg, _, _ := indexImage(t, "old")
	fi := &fakeImages{have: map[string]client.Image{"postgres:17": oldImg}, pullErr: errors.New("registry down")}
	r := &Runtime{images: fi, ns: "t"}
	got, err := r.RefreshImage(context.Background(), "postgres:17", "")
	if err == nil || got != "" {
		t.Fatalf("got %q, %v; want an error", got, err)
	}
	if fi.have["postgres:17"] != oldImg {
		t.Fatal("a failed pull replaced the local image")
	}
}

func TestPlatformDigestSingleManifest(t *testing.T) {
	img := &fakeImage{target: ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromString("m")}}
	got, err := platformDigest(context.Background(), img)
	if err != nil || got != digest.FromString("m").String() {
		t.Fatalf("got %q, %v", got, err)
	}
}
