package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultURL is the GitHub API endpoint of the latest release (PAAS_UPDATE_URL overrides it).
const DefaultURL = "https://api.github.com/repos/thesunbg/paas/releases/latest"

// SumsFile is the checksum list every release publishes next to its binaries
// (`sha256sum` output: "<hex>  <file>" per line), see .github/workflows/release.yml.
const SumsFile = "SHA256SUMS"

var (
	// ErrNoChecksum: the release has no SHA256SUMS, or it does not list the file.
	ErrNoChecksum = errors.New("the release publishes no SHA-256 checksum for this file")
	// ErrChecksumMismatch: the downloaded file is not the one the release lists.
	ErrChecksumMismatch = errors.New("checksum mismatch")
)

// Release is what the dashboard shows about a release and what a rollout needs from it.
type Release struct {
	Tag         string            `json:"tag"`
	Name        string            `json:"name"`
	Notes       string            `json:"notes"`
	URL         string            `json:"url"` // human page of the release
	PublishedAt time.Time         `json:"published_at"`
	Prerelease  bool              `json:"prerelease"`
	Assets      map[string]string `json:"assets"` // file name -> download URL
}

// BinaryName is the release asset of paasd for a Linux architecture (amd64, arm64).
func BinaryName(arch string) string { return "paasd-linux-" + arch }

// TagURL turns the "latest release" URL into the URL of one tag's release
// (GitHub: .../releases/latest -> .../releases/tags/<tag>).
func TagURL(latestURL, tag string) string {
	base := strings.TrimSuffix(strings.TrimRight(latestURL, "/"), "/latest")
	return base + "/tags/" + tag
}

// FetchRelease reads a release from the GitHub API (or anything that answers the same JSON).
func FetchRelease(ctx context.Context, c *http.Client, url string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release %s: %s", url, resp.Status)
	}
	var gh struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Prerelease  bool      `json:"prerelease"`
		Draft       bool      `json:"draft"`
		Assets      []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gh); err != nil {
		return nil, fmt.Errorf("release %s: %w", url, err)
	}
	if gh.TagName == "" || gh.Draft {
		return nil, fmt.Errorf("release %s: no published release", url)
	}
	if _, ok := Parse(gh.TagName); !ok {
		return nil, fmt.Errorf("release tag %q is not a semantic version", gh.TagName)
	}
	r := &Release{Tag: gh.TagName, Name: gh.Name, Notes: gh.Body, URL: gh.HTMLURL, PublishedAt: gh.PublishedAt,
		Prerelease: gh.Prerelease, Assets: map[string]string{}}
	for _, a := range gh.Assets {
		if a.Name != "" && a.URL != "" {
			r.Assets[a.Name] = a.URL
		}
	}
	return r, nil
}

// Checksums downloads and parses the release's SHA256SUMS. ErrNoChecksum when it has none.
func (r *Release) Checksums(ctx context.Context, c *http.Client) (map[string]string, error) {
	url := r.Assets[SumsFile]
	if url == "" {
		return nil, ErrNoChecksum
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", SumsFile, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return ParseSums(data), nil
}

// ParseSums reads `sha256sum` output ("<hex>  <name>" or "<hex> *<name>" per line).
// Malformed lines are skipped.
func ParseSums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		sum, name := strings.ToLower(f[0]), strings.TrimPrefix(f[1], "*")
		if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
			continue
		}
		out[filepath.Base(name)] = sum
	}
	return out
}

// Expected is the checksum a file must have. Without one the download is refused,
// unless allowUnverified (PAAS_UPDATE_ALLOW_UNVERIFIED=true, unsafe): then "" means
// "do not check".
func Expected(sums map[string]string, name string, allowUnverified bool) (string, error) {
	if s := sums[name]; s != "" {
		return s, nil
	}
	if allowUnverified {
		return "", nil
	}
	return "", fmt.Errorf("%s: %w (refusing to install it; PAAS_UPDATE_ALLOW_UNVERIFIED=true skips the check, unsafe)", name, ErrNoChecksum)
}

// Download fetches url into a new temporary file in dir, checks its SHA-256 against
// want (skipped when want is ""), flushes it to disk and makes it executable. It
// returns the file's path; on any error nothing is left behind.
func Download(ctx context.Context, c *http.Client, url, dir, want string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.CreateTemp(dir, ".paasd-download-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(path)
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fail(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); want != "" && !strings.EqualFold(got, want) {
		return fail(fmt.Errorf("%w for %s: got %s, want %s", ErrChecksumMismatch, url, got, want))
	}
	if err := f.Chmod(0o755); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// FileSHA256 is the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
