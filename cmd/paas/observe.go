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

// Alert rules and uptime checks (paas alerts / paas uptime).

// listFlag collects a repeatable flag (also accepts comma separated values).
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

// resolveChannels turns channel names or ids into ids of the project's team.
func resolveChannels(teamID string, refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	var out struct {
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
	}
	if err := call(http.MethodGet, "/teams/"+teamID+"/notifications", nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, ref := range refs {
		found := false
		for _, c := range out.Channels {
			if c.ID == ref || strings.EqualFold(c.Name, ref) {
				ids, found = append(ids, c.ID), true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("notification channel %q not found", ref)
		}
	}
	return ids, nil
}

// serviceRef resolves an optional service slug / name / id of the project.
func serviceRef(d *projectDetail, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	for _, s := range d.Services {
		if s.ID == ref || s.Slug == ref || strings.EqualFold(s.Name, ref) {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("service %q not found in project %s", ref, d.Project.Name)
}

func serviceSlug(d *projectDetail, id *string) string {
	if id == nil {
		return "(all)"
	}
	for _, s := range d.Services {
		if s.ID == *id {
			return s.Slug
		}
	}
	return *id
}

type alertRule struct {
	ID              string   `json:"id"`
	ServiceID       *string  `json:"service_id"`
	Name            string   `json:"name"`
	Metric          string   `json:"metric"`
	Comparator      string   `json:"comparator"`
	Threshold       float64  `json:"threshold"`
	ForSeconds      int      `json:"for_seconds"`
	CooldownSeconds int      `json:"cooldown_seconds"`
	ChannelIDs      []string `json:"channel_ids"`
	Enabled         bool     `json:"enabled"`
	States          []struct {
		ServiceName string   `json:"service_name"`
		State       string   `json:"state"`
		Value       *float64 `json:"value"`
	} `json:"states"`
}

func listAlerts(d *projectDetail) ([]alertRule, error) {
	var out struct {
		Rules []alertRule `json:"rules"`
	}
	err := call(http.MethodGet, "/projects/"+d.Project.ID+"/alerts", nil, &out)
	return out.Rules, err
}

func findAlert(d *projectDetail, ref string) (*alertRule, error) {
	rules, err := listAlerts(d)
	if err != nil {
		return nil, err
	}
	for i, r := range rules {
		if r.ID == ref || r.Name == ref {
			return &rules[i], nil
		}
	}
	return nil, fmt.Errorf("alert rule %q not found", ref)
}

func cmdAlerts(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	d, err := loadProject()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		rules, err := listAlerts(d)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSERVICE\tCONDITION\tFOR\tENABLED\tSTATE")
		for _, r := range rules {
			var states []string
			for _, st := range r.States {
				v := "-"
				if st.Value != nil {
					v = fmt.Sprintf("%.4g", *st.Value)
				}
				states = append(states, fmt.Sprintf("%s:%s(%s)", st.ServiceName, st.State, v))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s %g\t%s\t%v\t%s\n", r.ID[:8], r.Name, serviceSlug(d, r.ServiceID), r.Metric, r.Comparator, r.Threshold,
				time.Duration(r.ForSeconds)*time.Second, r.Enabled, strings.Join(states, " "))
		}
		return tw.Flush()
	case "add":
		pos, flags := splitFlags(args[1:])
		fs := flag.NewFlagSet("alerts add", flag.ExitOnError)
		metric := fs.String("metric", "", "cpu_percent | memory_percent | restarts | error_rate | instances")
		op := fs.String("op", ">", "comparator: > >= < <=")
		threshold := fs.Float64("threshold", 0, "threshold")
		forDur := fs.Duration("for", 5*time.Minute, "how long the condition must hold")
		cooldown := fs.Duration("cooldown", 30*time.Minute, "no new notification within this after the previous one")
		svc := fs.String("service", "", "service (default: every web/worker service of the project)")
		var channels listFlag
		fs.Var(&channels, "channel", "notification channel name or id (repeatable; default: channels subscribed to alert events)")
		_ = fs.Parse(flags)
		if *metric == "" || len(pos) > 1 {
			return errors.New("usage: paas alerts add [name] --metric cpu_percent --op '>' --threshold 80 [--for 5m] [--service svc] [--channel name]")
		}
		sid, err := serviceRef(d, *svc)
		if err != nil {
			return err
		}
		chIDs, err := resolveChannels(d.Project.TeamID, channels)
		if err != nil {
			return err
		}
		in := map[string]any{"metric": *metric, "comparator": *op, "threshold": *threshold,
			"for_seconds": int(forDur.Seconds()), "cooldown_seconds": int(cooldown.Seconds())}
		if len(pos) == 1 {
			in["name"] = pos[0]
		}
		if sid != "" {
			in["service_id"] = sid
		}
		if chIDs != nil {
			in["channel_ids"] = chIDs
		}
		var r alertRule
		if err := call(http.MethodPost, "/projects/"+d.Project.ID+"/alerts", in, &r); err != nil {
			return err
		}
		fmt.Printf("Created alert %q (%s): %s %s %g for %s\n", r.Name, r.ID, r.Metric, r.Comparator, r.Threshold, time.Duration(r.ForSeconds)*time.Second)
		return nil
	case "rm", "enable", "disable":
		if len(args) != 2 {
			return fmt.Errorf("usage: paas alerts %s <id or name>", args[0])
		}
		r, err := findAlert(d, args[1])
		if err != nil {
			return err
		}
		if args[0] == "rm" {
			return call(http.MethodDelete, "/alerts/"+r.ID, nil, nil)
		}
		return call(http.MethodPatch, "/alerts/"+r.ID, map[string]bool{"enabled": args[0] == "enable"}, nil)
	}
	return errors.New("unknown alerts command (ls, add, rm, enable, disable)")
}

type uptimeCheck struct {
	ID               string     `json:"id"`
	ServiceID        *string    `json:"service_id"`
	Name             string     `json:"name"`
	URL              string     `json:"url"`
	Method           string     `json:"method"`
	IntervalSeconds  int        `json:"interval_seconds"`
	Enabled          bool       `json:"enabled"`
	State            string     `json:"state"`
	LastError        string     `json:"last_error"`
	CertExpiresAt    *time.Time `json:"cert_expires_at"`
	FailureThreshold int        `json:"failure_threshold"`
	Stats            struct {
		Uptime24h *float64 `json:"uptime_24h"`
		Uptime7d  *float64 `json:"uptime_7d"`
		AvgMS     *float64 `json:"avg_latency_ms_24h"`
		P95MS     *float64 `json:"p95_latency_ms_24h"`
	} `json:"stats"`
}

func pct(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", *v)
}

func ms(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f ms", *v)
}

func listUptime(d *projectDetail) ([]uptimeCheck, error) {
	var out struct {
		Checks []uptimeCheck `json:"checks"`
	}
	err := call(http.MethodGet, "/projects/"+d.Project.ID+"/uptime", nil, &out)
	return out.Checks, err
}

func findUptime(d *projectDetail, ref string) (*uptimeCheck, error) {
	checks, err := listUptime(d)
	if err != nil {
		return nil, err
	}
	for i, c := range checks {
		if c.ID == ref || c.Name == ref || c.URL == ref {
			return &checks[i], nil
		}
	}
	return nil, fmt.Errorf("uptime check %q not found", ref)
}

func cmdUptime(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	d, err := loadProject()
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls":
		checks, err := listUptime(d)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tURL\tSTATE\t24H\t7D\tP95\tCERT")
		for _, c := range checks {
			state := c.State
			if !c.Enabled {
				state = "paused"
			}
			cert := "-"
			if c.CertExpiresAt != nil {
				cert = fmt.Sprintf("%dd", int(time.Until(*c.CertExpiresAt).Hours()/24))
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID[:8], c.Name, c.URL, state, pct(c.Stats.Uptime24h), pct(c.Stats.Uptime7d), ms(c.Stats.P95MS), cert)
		}
		return tw.Flush()
	case "add":
		pos, flags := splitFlags(args[1:])
		fs := flag.NewFlagSet("uptime add", flag.ExitOnError)
		name := fs.String("name", "", "display name (default: the host)")
		method := fs.String("method", "GET", "GET | HEAD | POST")
		expect := fs.Int("expect", 0, "expected HTTP status (default: any 2xx/3xx)")
		keyword := fs.String("keyword", "", "text the response body must contain")
		interval := fs.Duration("interval", time.Minute, "check interval (at least 30s)")
		timeout := fs.Duration("timeout", 10*time.Second, "request timeout")
		failures := fs.Int("failures", 3, "consecutive failures before alerting")
		svc := fs.String("service", "", "attach the check to a service")
		var channels listFlag
		fs.Var(&channels, "channel", "notification channel name or id (repeatable; default: channels subscribed to uptime events)")
		_ = fs.Parse(flags)
		if len(pos) != 1 {
			return errors.New("usage: paas uptime add <url> [--interval 60s] [--expect 200] [--keyword K] [--failures 3] [--service svc] [--channel name]")
		}
		sid, err := serviceRef(d, *svc)
		if err != nil {
			return err
		}
		chIDs, err := resolveChannels(d.Project.TeamID, channels)
		if err != nil {
			return err
		}
		in := map[string]any{"url": pos[0], "name": *name, "method": *method, "expected_status": *expect, "keyword": *keyword,
			"interval_seconds": int(interval.Seconds()), "timeout_seconds": int(timeout.Seconds()), "failure_threshold": *failures}
		if sid != "" {
			in["service_id"] = sid
		}
		if chIDs != nil {
			in["channel_ids"] = chIDs
		}
		var c uptimeCheck
		if err := call(http.MethodPost, "/projects/"+d.Project.ID+"/uptime", in, &c); err != nil {
			return err
		}
		fmt.Printf("Created uptime check %q (%s): %s %s every %s\n", c.Name, c.ID, c.Method, c.URL, time.Duration(c.IntervalSeconds)*time.Second)
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: paas uptime show <id, name or url>")
		}
		c, err := findUptime(d, args[1])
		if err != nil {
			return err
		}
		var out struct {
			Check   uptimeCheck `json:"check"`
			Results []struct {
				TS         time.Time `json:"ts"`
				OK         bool      `json:"ok"`
				StatusCode int       `json:"status_code"`
				LatencyMS  int       `json:"latency_ms"`
				Error      string    `json:"error"`
			} `json:"results"`
		}
		if err := call(http.MethodGet, "/uptime/"+c.ID+"?limit=20", nil, &out); err != nil {
			return err
		}
		k := out.Check
		fmt.Printf("%s  %s %s  state: %s\n", k.Name, k.Method, k.URL, k.State)
		fmt.Printf("uptime 24h %s · 7d %s · latency avg %s · p95 %s\n", pct(k.Stats.Uptime24h), pct(k.Stats.Uptime7d), ms(k.Stats.AvgMS), ms(k.Stats.P95MS))
		if k.CertExpiresAt != nil {
			fmt.Printf("certificate expires %s\n", k.CertExpiresAt.Local().Format("2006-01-02"))
		}
		if k.LastError != "" {
			fmt.Println("last error:", k.LastError)
		}
		fmt.Println()
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tRESULT\tSTATUS\tLATENCY\tERROR")
		for _, r := range out.Results {
			res := "ok"
			if !r.OK {
				res = "FAIL"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d ms\t%s\n", r.TS.Local().Format("01-02 15:04:05"), res, r.StatusCode, r.LatencyMS, truncate(r.Error, 60))
		}
		return tw.Flush()
	case "rm", "enable", "disable":
		if len(args) != 2 {
			return fmt.Errorf("usage: paas uptime %s <id, name or url>", args[0])
		}
		c, err := findUptime(d, args[1])
		if err != nil {
			return err
		}
		if args[0] == "rm" {
			return call(http.MethodDelete, "/uptime/"+c.ID, nil, nil)
		}
		return call(http.MethodPatch, "/uptime/"+c.ID, map[string]bool{"enabled": args[0] == "enable"}, nil)
	}
	return errors.New("unknown uptime command (ls, add, show, rm, enable, disable)")
}
