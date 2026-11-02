package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

type scanPolicy struct {
	Mode      string `json:"mode"`
	Threshold string `json:"threshold"`
}

type scanFinding struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Pkg       string `json:"pkg"`
	Installed string `json:"installed"`
	Fixed     string `json:"fixed"`
	Title     string `json:"title"`
}

type imageScan struct {
	DeploymentID   string        `json:"deployment_id"`
	Image          string        `json:"image"`
	Status         string        `json:"status"`
	ScannerVersion string        `json:"scanner_version"`
	DBUpdatedAt    *time.Time    `json:"db_updated_at"`
	OS             string        `json:"os"`
	Critical       int           `json:"critical"`
	High           int           `json:"high"`
	Medium         int           `json:"medium"`
	Low            int           `json:"low"`
	Unknown        int           `json:"unknown"`
	Findings       []scanFinding `json:"findings"`
	PolicyMode     string        `json:"policy_mode"`
	Threshold      string        `json:"threshold"`
	Blocked        bool          `json:"blocked"`
	Message        string        `json:"message"`
	ScannedAt      time.Time     `json:"scanned_at"`
}

// cmdScan: `paas scan <service> [--all]` shows the latest vulnerability scan;
// `paas scan policy [off|warn|block] [--threshold SEV]` shows or sets the project's policy.
func cmdScan(args []string) error {
	pos, flags := splitFlags(args)
	if len(pos) >= 1 && pos[0] == "policy" {
		return cmdScanPolicy(pos[1:], flags)
	}
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	all := fs.Bool("all", false, "list every stored finding (default: the 20 most severe)")
	_ = fs.Parse(flags)
	if len(pos) != 1 {
		return errors.New("usage: paas scan <service> [--all] | paas scan policy [off|warn|block] [--threshold CRITICAL|HIGH|MEDIUM|LOW]")
	}
	s, _, err := findService(pos[0])
	if err != nil {
		return err
	}
	var out struct {
		Scan   *imageScan `json:"scan"`
		Policy scanPolicy `json:"policy"`
	}
	if err := call(http.MethodGet, "/services/"+s.ID+"/scan", nil, &out); err != nil {
		return err
	}
	fmt.Printf("Policy: %s\n", policyText(out.Policy))
	x := out.Scan
	if x == nil {
		fmt.Println("No vulnerability scan yet: the next deployment is scanned.")
		return nil
	}
	fmt.Printf("Deployment %s  %s\n", short(x.DeploymentID), x.Image)
	fmt.Printf("Scanned %s", x.ScannedAt.Local().Format("2006-01-02 15:04"))
	if x.ScannerVersion != "" {
		fmt.Printf(" with trivy %s", x.ScannerVersion)
	}
	if x.DBUpdatedAt != nil {
		fmt.Printf(" (vulnerability DB of %s)", x.DBUpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	if x.OS != "" {
		fmt.Printf(", base %s", x.OS)
	}
	fmt.Println()
	if x.Message != "" {
		fmt.Println(x.Message)
	}
	if x.Status != "scanned" {
		return nil
	}
	fmt.Printf("\nCRITICAL %d  HIGH %d  MEDIUM %d  LOW %d", x.Critical, x.High, x.Medium, x.Low)
	if x.Unknown > 0 {
		fmt.Printf("  UNKNOWN %d", x.Unknown)
	}
	fmt.Println()
	if len(x.Findings) == 0 {
		return nil
	}
	list := x.Findings
	if !*all && len(list) > 20 {
		list = list[:20]
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tID\tPACKAGE\tINSTALLED\tFIXED\tTITLE")
	for _, f := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Severity, f.ID, f.Pkg, f.Installed, firstOr(f.Fixed, "-"), clipText(f.Title, 60))
	}
	_ = tw.Flush()
	total := x.Critical + x.High + x.Medium + x.Low + x.Unknown
	if len(list) < total {
		hint := ""
		if !*all && len(x.Findings) > len(list) {
			hint = " (--all lists the stored ones)"
		}
		fmt.Printf("\n%d of %d findings shown%s\n", len(list), total, hint)
	}
	return nil
}

func cmdScanPolicy(pos, flags []string) error {
	fs := flag.NewFlagSet("scan policy", flag.ExitOnError)
	threshold := fs.String("threshold", "", "lowest severity that counts: CRITICAL, HIGH, MEDIUM or LOW")
	_ = fs.Parse(flags)
	p, err := currentProject()
	if err != nil {
		return err
	}
	path := "/projects/" + p.ID + "/scan-policy"
	var pol scanPolicy
	if len(pos) == 0 && *threshold == "" {
		if err := call(http.MethodGet, path, nil, &pol); err != nil {
			return err
		}
		fmt.Printf("Project %s: %s\n", p.Name, policyText(pol))
		return nil
	}
	if len(pos) > 1 {
		return errors.New("usage: paas scan policy [off|warn|block] [--threshold CRITICAL|HIGH|MEDIUM|LOW]")
	}
	if err := call(http.MethodGet, path, nil, &pol); err != nil {
		return err
	}
	if len(pos) == 1 {
		pol.Mode = pos[0]
	}
	if *threshold != "" {
		pol.Threshold = strings.ToUpper(*threshold)
	}
	if err := call(http.MethodPut, path, pol, &pol); err != nil {
		return err
	}
	fmt.Printf("Project %s: %s\n", p.Name, policyText(pol))
	return nil
}

func policyText(p scanPolicy) string {
	switch p.Mode {
	case "off":
		return "off (images are not scanned)"
	case "block":
		return fmt.Sprintf("block deployments with %s or worse findings", p.Threshold)
	default:
		return fmt.Sprintf("warn on %s or worse findings (deployments go on)", p.Threshold)
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func firstOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
