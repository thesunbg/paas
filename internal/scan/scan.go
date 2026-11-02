// Package scan checks container images for known vulnerabilities with Trivy
// (an external binary on build nodes) and decides what a project's scan policy
// makes of the result.
package scan

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/thesunbg/paas/internal/proto"
)

// Policy modes.
const (
	ModeOff   = "off"   // no scan
	ModeWarn  = "warn"  // scan, record and notify; always deploy
	ModeBlock = "block" // fail the deployment on findings at or above the threshold
)

// Severities from most to least severe (Trivy's names). UNKNOWN never counts
// towards a threshold.
var Severities = []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"}

// Policy is a project's scan policy.
type Policy struct {
	Mode      string `json:"mode"`
	Threshold string `json:"threshold"` // one of Severities
}

// DefaultPolicy applies to projects that never set one.
func DefaultPolicy() Policy { return Policy{Mode: ModeWarn, Threshold: "CRITICAL"} }

// Normalize lower-cases the mode, upper-cases the threshold, fills defaults and validates.
func (p Policy) Normalize() (Policy, error) {
	p.Mode = strings.ToLower(strings.TrimSpace(p.Mode))
	p.Threshold = strings.ToUpper(strings.TrimSpace(p.Threshold))
	if p.Mode == "" {
		p.Mode = ModeWarn
	}
	if p.Threshold == "" {
		p.Threshold = "CRITICAL"
	}
	switch p.Mode {
	case ModeOff, ModeWarn, ModeBlock:
	default:
		return p, fmt.Errorf("mode must be off, warn or block")
	}
	if rank(p.Threshold) < 0 {
		return p, fmt.Errorf("threshold must be one of %s", strings.Join(Severities, ", "))
	}
	return p, nil
}

// rank is 0 for CRITICAL ... 3 for LOW, -1 for anything else.
func rank(sev string) int {
	for i, s := range Severities {
		if s == sev {
			return i
		}
	}
	return -1
}

// AtOrAbove counts the findings of r at or above the threshold severity.
func AtOrAbove(r *proto.ScanResult, threshold string) int {
	counts := []int{r.Critical, r.High, r.Medium, r.Low}
	n := 0
	for i := 0; i <= rank(threshold); i++ {
		n += counts[i]
	}
	return n
}

// Summary is "2 critical, 5 high, 10 medium, 3 low".
func Summary(r *proto.ScanResult) string {
	s := fmt.Sprintf("%d critical, %d high, %d medium, %d low", r.Critical, r.High, r.Medium, r.Low)
	if r.Unknown > 0 {
		s += fmt.Sprintf(", %d unknown", r.Unknown)
	}
	return s
}

// Verdict is what a deployment does after its scan.
type Verdict struct {
	Block  bool   // fail the deployment before it goes live
	Notify bool   // findings at or above the threshold: send the notification
	Count  int    // findings at or above the threshold
	Log    string // line for the deployment log (without the "==> " prefix)
}

// Decide applies a policy to a scan. res is nil when no scan could be made;
// unavailable then says why (no scanner on the node, scan failed, no build node).
// Without a scan, warn deploys and block fails closed.
func Decide(p Policy, res *proto.ScanResult, unavailable string) Verdict {
	if p.Mode == ModeOff {
		return Verdict{}
	}
	if res == nil {
		if unavailable == "" {
			unavailable = "no result"
		}
		if p.Mode == ModeBlock {
			return Verdict{Block: true, Log: "vulnerability scan is required by the project policy (block) but could not run: " + unavailable}
		}
		return Verdict{Log: "vulnerability scan skipped: " + unavailable}
	}
	n := AtOrAbove(res, p.Threshold)
	v := Verdict{Count: n, Notify: n > 0}
	switch {
	case n == 0:
		v.Log = fmt.Sprintf("vulnerability scan: %s (none at or above %s)", Summary(res), p.Threshold)
	case p.Mode == ModeBlock:
		v.Block = true
		v.Log = fmt.Sprintf("blocked by the vulnerability policy: %d finding(s) at or above %s (%s)", n, p.Threshold, Summary(res))
	default:
		v.Log = fmt.Sprintf("vulnerability scan: %d finding(s) at or above %s (%s); deploying anyway (policy: warn)", n, p.Threshold, Summary(res))
	}
	return v
}

// trivyReport is the part of `trivy image --format json` we read (schema version 2).
type trivyReport struct {
	SchemaVersion int `json:"SchemaVersion"`
	Metadata      struct {
		OS *struct {
			Family string `json:"Family"`
			Name   string `json:"Name"`
		} `json:"OS"`
	} `json:"Metadata"`
	Results []struct {
		Target          string `json:"Target"`
		Vulnerabilities []struct {
			VulnerabilityID  string `json:"VulnerabilityID"`
			PkgName          string `json:"PkgName"`
			InstalledVersion string `json:"InstalledVersion"`
			FixedVersion     string `json:"FixedVersion"`
			Title            string `json:"Title"`
			Severity         string `json:"Severity"`
			PrimaryURL       string `json:"PrimaryURL"`
		} `json:"Vulnerabilities"`
	} `json:"Results"`
}

// ParseTrivy turns a Trivy JSON report into counts by severity and the most
// severe findings (at most proto.MaxScanFindings). A vulnerability reported
// twice for the same package version (two targets) counts once.
func ParseTrivy(data []byte) (*proto.ScanResult, error) {
	var rep trivyReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("parse trivy report: %w", err)
	}
	if rep.SchemaVersion != 2 {
		return nil, fmt.Errorf("unsupported trivy report schema version %d", rep.SchemaVersion)
	}
	res := &proto.ScanResult{Scanner: "trivy", Findings: []proto.ScanFinding{}}
	if osInfo := rep.Metadata.OS; osInfo != nil {
		res.OS = strings.TrimSpace(osInfo.Family + " " + osInfo.Name)
	}
	seen := map[string]bool{}
	var all []proto.ScanFinding
	for _, r := range rep.Results {
		for _, v := range r.Vulnerabilities {
			key := v.VulnerabilityID + "\x00" + v.PkgName + "\x00" + v.InstalledVersion
			if seen[key] {
				continue
			}
			seen[key] = true
			sev := strings.ToUpper(v.Severity)
			switch sev {
			case "CRITICAL":
				res.Critical++
			case "HIGH":
				res.High++
			case "MEDIUM":
				res.Medium++
			case "LOW":
				res.Low++
			default:
				sev = "UNKNOWN"
				res.Unknown++
			}
			all = append(all, proto.ScanFinding{
				ID: v.VulnerabilityID, Severity: sev, Pkg: v.PkgName, Installed: v.InstalledVersion,
				Fixed: v.FixedVersion, Title: clip(v.Title, 300), Target: clip(r.Target, 200), URL: clip(v.PrimaryURL, 300),
			})
		}
	}
	// Most severe first; within a severity, fixable first (actionable), then newest id.
	sort.SliceStable(all, func(i, j int) bool {
		ri, rj := sevOrder(all[i].Severity), sevOrder(all[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if (all[i].Fixed != "") != (all[j].Fixed != "") {
			return all[i].Fixed != ""
		}
		if all[i].ID != all[j].ID {
			return all[i].ID > all[j].ID
		}
		return all[i].Pkg < all[j].Pkg
	})
	if len(all) > proto.MaxScanFindings {
		all = all[:proto.MaxScanFindings]
	}
	res.Findings = append(res.Findings, all...)
	return res, nil
}

func sevOrder(sev string) int {
	if r := rank(sev); r >= 0 {
		return r
	}
	return len(Severities)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Do not cut a UTF-8 sequence in half.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
