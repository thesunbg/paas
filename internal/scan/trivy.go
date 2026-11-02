package scan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/thesunbg/paas/internal/proto"
)

// ErrMissing means the node has no scanner binary.
var ErrMissing = errors.New("trivy is not installed on this node")

// DefaultDBMaxAge is how long a downloaded vulnerability DB is used before the
// next scan refreshes it. Trivy publishes a new DB every 6 hours; scanning
// against a copy up to 12 hours old keeps scans fast (no download on the hot
// path) and is still current enough to gate deployments.
const DefaultDBMaxAge = 12 * time.Hour

// retryAfterFailure spaces out DB refresh attempts while the download fails
// (offline node, rate limit), so every scan does not wait for a timeout.
const retryAfterFailure = time.Hour

// Trivy runs the trivy binary against image archives. Scans on one node are
// serialised: they share the DB and the layer cache in CacheDir.
type Trivy struct {
	Bin      string        // binary name or path (default "trivy")
	CacheDir string        // DB and layer cache, kept across scans
	MaxDBAge time.Duration // default DefaultDBMaxAge
	Timeout  time.Duration // one scan (default 10 min)

	mu          sync.Mutex
	version     string
	lastFailure time.Time
	now         func() time.Time
}

// NewTrivy returns a scanner caching its DB under cacheDir.
func NewTrivy(cacheDir string) *Trivy {
	return &Trivy{Bin: "trivy", CacheDir: cacheDir}
}

func (t *Trivy) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// Path returns the scanner binary, or ErrMissing.
func (t *Trivy) Path() (string, error) {
	bin := t.Bin
	if bin == "" {
		bin = "trivy"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", ErrMissing
	}
	return p, nil
}

// dbMeta is <cache>/db/metadata.json (also java-db/metadata.json) as Trivy writes it.
type dbMeta struct {
	UpdatedAt    time.Time `json:"UpdatedAt"`
	DownloadedAt time.Time `json:"DownloadedAt"`
}

func readMeta(path string) (*dbMeta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m dbMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// checkedStamp records when we last asked Trivy to refresh the DB: Trivy leaves
// metadata.json alone when its copy is not due yet, so DownloadedAt alone would
// make every scan ask again.
func (t *Trivy) checkedStamp() string { return filepath.Join(t.CacheDir, "paas-db-checked") }

// dbFresh reports whether the vulnerability DB in the cache is younger than the max age.
func (t *Trivy) dbFresh() (fresh bool, exists bool) {
	maxAge := t.MaxDBAge
	if maxAge <= 0 {
		maxAge = DefaultDBMaxAge
	}
	m, err := readMeta(filepath.Join(t.CacheDir, "db", "metadata.json"))
	if err != nil {
		return false, false
	}
	last := m.DownloadedAt
	if st, err := os.Stat(t.checkedStamp()); err == nil && st.ModTime().After(last) {
		last = st.ModTime()
	}
	return t.clock().Sub(last) < maxAge, true
}

// ensureDB refreshes the vulnerability DB when it is older than the max age. A
// failed refresh with a DB already in the cache scans against that copy.
func (t *Trivy) ensureDB(ctx context.Context, bin string, log func(string)) error {
	fresh, exists := t.dbFresh()
	if fresh {
		return nil
	}
	if exists && t.clock().Sub(t.lastFailure) < retryAfterFailure {
		return nil
	}
	log("==> updating the vulnerability database")
	if err := os.MkdirAll(t.CacheDir, 0o700); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_, stderr, err := t.run(cctx, bin, "image", "--download-db-only", "--cache-dir", t.CacheDir, "--quiet")
	if err != nil {
		t.lastFailure = t.clock()
		if exists {
			log("warning: could not update the vulnerability database, scanning with the cached copy: " + errText(err, stderr))
			return nil
		}
		return fmt.Errorf("download vulnerability database: %s", errText(err, stderr))
	}
	now := t.clock()
	_ = os.WriteFile(t.checkedStamp(), nil, 0o600)
	_ = os.Chtimes(t.checkedStamp(), now, now)
	return nil
}

func (t *Trivy) run(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	// Env, not flags: older or newer releases ignore what they do not know.
	cmd.Env = append(os.Environ(), "TRIVY_NO_PROGRESS=true", "TRIVY_SKIP_VERSION_CHECK=true", "TRIVY_DISABLE_TELEMETRY=true")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	return out.Bytes(), stderr.Bytes(), err
}

func errText(err error, stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	if s := strings.TrimSpace(strings.Join(lines, " ")); s != "" {
		return err.Error() + ": " + s
	}
	return err.Error()
}

// Version is the scanner version ("0.65.0"), cached after the first call.
func (t *Trivy) Version(ctx context.Context) string {
	if t.version != "" {
		return t.version
	}
	bin, err := t.Path()
	if err != nil {
		return ""
	}
	out, _, err := t.run(ctx, bin, "--version", "--format", "json")
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"Version"`
	}
	if json.Unmarshal(out, &v) == nil {
		t.version = v.Version
	}
	return t.version
}

// ScanArchive scans an image archive (OCI or docker-archive tar) for OS and
// language package vulnerabilities. The scan itself is offline: it uses the
// cached DB (--skip-db-update) and never resolves dependencies over the network
// (--offline-scan), so image contents cannot make the node reach anything.
func (t *Trivy) ScanArchive(ctx context.Context, archive string, log func(string)) (*proto.ScanResult, error) {
	bin, err := t.Path()
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureDB(ctx, bin, log); err != nil {
		return nil, err
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"image", "--input", archive, "--format", "json", "--scanners", "vuln",
		"--skip-db-update", "--offline-scan", "--cache-dir", t.CacheDir, "--quiet", "--timeout", timeout.String()}
	// The Java DB (for jar files) is fetched by Trivy on the first scan that needs it;
	// once present, it follows the same refresh rule as the main DB.
	if m, err := readMeta(filepath.Join(t.CacheDir, "java-db", "metadata.json")); err == nil {
		maxAge := t.MaxDBAge
		if maxAge <= 0 {
			maxAge = DefaultDBMaxAge
		}
		if t.clock().Sub(m.DownloadedAt) < maxAge {
			args = append(args, "--skip-java-db-update")
		}
	}
	start := t.clock()
	out, stderr, err := t.run(cctx, bin, args...)
	if err != nil {
		return nil, fmt.Errorf("trivy: %s", errText(err, stderr))
	}
	res, err := ParseTrivy(out)
	if err != nil {
		return nil, err
	}
	res.ScannerVersion = t.Version(ctx)
	res.ScannedAt = t.clock().UTC()
	if m, err := readMeta(filepath.Join(t.CacheDir, "db", "metadata.json")); err == nil && !m.UpdatedAt.IsZero() {
		u := m.UpdatedAt.UTC()
		res.DBUpdatedAt = &u
	}
	log(fmt.Sprintf("==> scanned in %s", t.clock().Sub(start).Round(100*time.Millisecond)))
	return res, nil
}
