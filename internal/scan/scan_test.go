package scan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/proto"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/trivy-node-alpine.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseTrivy(t *testing.T) {
	res, err := ParseTrivy(loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Critical != 1 || res.High != 2 || res.Medium != 3 || res.Low != 1 || res.Unknown != 1 {
		t.Fatalf("counts = %s", Summary(res))
	}
	if res.OS != "alpine 3.19.1" || res.Scanner != "trivy" {
		t.Fatalf("os=%q scanner=%q", res.OS, res.Scanner)
	}
	var got []string
	for _, f := range res.Findings {
		got = append(got, f.Severity+":"+f.ID+":"+f.Pkg)
	}
	want := []string{
		"CRITICAL:CVE-2021-44906:minimist",
		"HIGH:CVE-2022-24999:qs",            // fixable first
		"HIGH:GHSA-xxxx-yyyy-zzzz:left-pad", // no fix
		"MEDIUM:CVE-2024-0727:libcrypto3",
		"MEDIUM:CVE-2024-0727:libssl3",
		"MEDIUM:CVE-2023-42363:busybox",
		"LOW:CVE-2024-2511:libssl3",
		"UNKNOWN:CVE-2025-0001:zlib",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings order:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	f := res.Findings[0]
	if f.Installed != "1.2.5" || f.Fixed != "1.2.6, 0.2.4" || f.Target != "app/package-lock.json" || f.Title != "minimist: prototype pollution" || f.URL == "" {
		t.Fatalf("finding = %+v", f)
	}
}

func TestParseTrivyDedupAndBound(t *testing.T) {
	type vuln struct {
		VulnerabilityID, PkgName, InstalledVersion, Severity string
	}
	var vulns []vuln
	for i := 0; i < 300; i++ {
		vulns = append(vulns, vuln{fmt.Sprintf("CVE-2020-%04d", i), "pkg", "1.0", []string{"LOW", "HIGH"}[i%2]})
	}
	report := map[string]any{"SchemaVersion": 2, "Results": []any{
		map[string]any{"Target": "a", "Vulnerabilities": vulns},
		map[string]any{"Target": "b", "Vulnerabilities": vulns[:10]}, // same package versions again
	}}
	b, _ := json.Marshal(report)
	res, err := ParseTrivy(b)
	if err != nil {
		t.Fatal(err)
	}
	if res.High != 150 || res.Low != 150 {
		t.Fatalf("counts = %s (duplicates must count once)", Summary(res))
	}
	if len(res.Findings) != proto.MaxScanFindings {
		t.Fatalf("findings = %d, want %d", len(res.Findings), proto.MaxScanFindings)
	}
	if res.Findings[149].Severity != "HIGH" || res.Findings[150].Severity != "LOW" {
		t.Fatal("the bound must keep the most severe findings")
	}
}

func TestParseTrivyErrors(t *testing.T) {
	if _, err := ParseTrivy([]byte("not json")); err == nil {
		t.Fatal("want error for garbage")
	}
	if _, err := ParseTrivy([]byte(`{"SchemaVersion":1}`)); err == nil {
		t.Fatal("want error for an unknown schema")
	}
	res, err := ParseTrivy([]byte(`{"SchemaVersion":2,"Results":[]}`))
	if err != nil || res.Findings == nil || len(res.Findings) != 0 {
		t.Fatalf("clean image: %+v %v", res, err)
	}
}

func TestPolicyNormalize(t *testing.T) {
	p, err := Policy{Mode: " Block ", Threshold: "high"}.Normalize()
	if err != nil || p.Mode != ModeBlock || p.Threshold != "HIGH" {
		t.Fatalf("got %+v %v", p, err)
	}
	if p, err := (Policy{}).Normalize(); err != nil || p != DefaultPolicy() {
		t.Fatalf("empty policy = %+v %v, want the default", p, err)
	}
	for _, bad := range []Policy{{Mode: "deny"}, {Mode: "warn", Threshold: "UNKNOWN"}, {Mode: "warn", Threshold: "severe"}} {
		if _, err := bad.Normalize(); err == nil {
			t.Fatalf("%+v: want error", bad)
		}
	}
}

func TestDecide(t *testing.T) {
	res := &proto.ScanResult{Critical: 0, High: 2, Medium: 3, Low: 1, Unknown: 4}
	cases := []struct {
		p            Policy
		block, alert bool
		count        int
		log          string
	}{
		{Policy{ModeWarn, "CRITICAL"}, false, false, 0, "none at or above CRITICAL"},
		{Policy{ModeBlock, "CRITICAL"}, false, false, 0, "none at or above CRITICAL"},
		{Policy{ModeWarn, "HIGH"}, false, true, 2, "deploying anyway"},
		{Policy{ModeBlock, "HIGH"}, true, true, 2, "blocked by the vulnerability policy: 2 finding(s) at or above HIGH"},
		{Policy{ModeBlock, "MEDIUM"}, true, true, 5, "5 finding(s)"},
		{Policy{ModeBlock, "LOW"}, true, true, 6, "6 finding(s)"}, // UNKNOWN never counts
		{Policy{ModeOff, "LOW"}, false, false, 0, ""},
	}
	for _, c := range cases {
		v := Decide(c.p, res, "")
		if v.Block != c.block || v.Notify != c.alert || v.Count != c.count || !strings.Contains(v.Log, c.log) {
			t.Errorf("%+v: got %+v", c.p, v)
		}
	}
}

// No scanner on the node: warn deploys with a log line, block fails closed.
func TestDecideScannerMissing(t *testing.T) {
	v := Decide(Policy{ModeWarn, "CRITICAL"}, nil, ErrMissing.Error())
	if v.Block || v.Notify || !strings.Contains(v.Log, "skipped: trivy is not installed") {
		t.Fatalf("warn: %+v", v)
	}
	v = Decide(Policy{ModeBlock, "CRITICAL"}, nil, ErrMissing.Error())
	if !v.Block || !strings.Contains(v.Log, "required by the project policy (block)") || !strings.Contains(v.Log, "trivy is not installed") {
		t.Fatalf("block: %+v", v)
	}
	if v := Decide(Policy{ModeOff, "CRITICAL"}, nil, ErrMissing.Error()); v.Block || v.Log != "" {
		t.Fatalf("off: %+v", v)
	}
}

func TestTrivyMissingBinary(t *testing.T) {
	tv := &Trivy{Bin: filepath.Join(t.TempDir(), "no-such-trivy"), CacheDir: t.TempDir()}
	if _, err := tv.Path(); !errors.Is(err, ErrMissing) {
		t.Fatalf("Path: %v", err)
	}
	if _, err := tv.ScanArchive(context.Background(), "/nonexistent.tar", func(string) {}); !errors.Is(err, ErrMissing) {
		t.Fatalf("ScanArchive: %v", err)
	}
}

// fakeTrivy writes a shell script standing in for trivy: it logs its arguments,
// "downloads" the DB on --download-db-only and prints the fixture on a scan.
func fakeTrivy(t *testing.T) (bin, argsLog string) {
	t.Helper()
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args.log")
	fixture, _ := filepath.Abs("testdata/trivy-node-alpine.json")
	script := `#!/bin/sh
echo "$*" >> ` + argsLog + `
case "$*" in
  *--version*) echo '{"Version":"0.65.0"}'; exit 0;;
  *--download-db-only*)
    cache=""; prev=""
    for a in "$@"; do [ "$prev" = "--cache-dir" ] && cache="$a"; prev="$a"; done
    mkdir -p "$cache/db"
    echo '{"Version":2,"UpdatedAt":"2026-10-30T06:00:00Z","NextUpdate":"2026-10-31T06:00:00Z","DownloadedAt":"2026-10-30T07:00:00Z"}' > "$cache/db/metadata.json"
    exit 0;;
  *--input*) cat ` + fixture + `; exit 0;;
esac
exit 3
`
	bin = filepath.Join(dir, "trivy")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsLog
}

func TestTrivyScanCachesDB(t *testing.T) {
	bin, argsLog := fakeTrivy(t)
	cache := t.TempDir()
	now := time.Now()
	tv := &Trivy{Bin: bin, CacheDir: cache, now: func() time.Time { return now }}
	var logs []string
	logf := func(l string) { logs = append(logs, l) }

	res, err := tv.ScanArchive(context.Background(), "/tmp/image.tar", logf)
	if err != nil {
		t.Fatal(err)
	}
	if res.Critical != 1 || res.ScannerVersion != "0.65.0" || res.DBUpdatedAt == nil || res.ScannedAt.IsZero() {
		t.Fatalf("result = %+v", res)
	}
	// Second scan right after: the DB is fresh, no download.
	if _, err := tv.ScanArchive(context.Background(), "/tmp/image.tar", logf); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsLog)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	downloads, scans := 0, 0
	for _, c := range calls {
		if strings.Contains(c, "--download-db-only") {
			downloads++
		}
		if strings.Contains(c, "--input /tmp/image.tar") {
			scans++
			for _, f := range []string{"--format json", "--scanners vuln", "--skip-db-update", "--offline-scan", "--cache-dir " + cache} {
				if !strings.Contains(c, f) {
					t.Errorf("scan args %q lack %q", c, f)
				}
			}
		}
	}
	if downloads != 1 || scans != 2 {
		t.Fatalf("downloads=%d scans=%d, calls:\n%s", downloads, scans, b)
	}

	// 13 hours later the DB is refreshed once more.
	now = now.Add(13 * time.Hour)
	if _, err := tv.ScanArchive(context.Background(), "/tmp/image.tar", logf); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(argsLog)
	if n := strings.Count(string(b), "--download-db-only"); n != 2 {
		t.Fatalf("downloads after 13h = %d, want 2", n)
	}
}

func TestTrivyDBDownloadFails(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "trivy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'FATAL: network unreachable' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tv := &Trivy{Bin: bin, CacheDir: t.TempDir()}
	_, err := tv.ScanArchive(context.Background(), "/tmp/image.tar", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "download vulnerability database") || !strings.Contains(err.Error(), "network unreachable") {
		t.Fatalf("err = %v", err)
	}
}
