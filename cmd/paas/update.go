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

type updateRelease struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	Notes       string    `json:"notes"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
}

type updateStep struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type updateRollout struct {
	Version     string       `json:"version"`
	Status      string       `json:"status"`
	Message     string       `json:"message"`
	Steps       []updateStep `json:"steps"`
	RequestedBy string       `json:"requested_by"`
	CreatedAt   time.Time    `json:"created_at"`
}

type updateInfo struct {
	Version         string         `json:"version"`
	Development     bool           `json:"development"`
	CheckEnabled    bool           `json:"check_enabled"`
	Latest          *updateRelease `json:"latest"`
	CheckedAt       *time.Time     `json:"checked_at"`
	CheckError      string         `json:"check_error"`
	UpdateAvailable bool           `json:"update_available"`
	ControlPlanes   []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Arch    string `json:"arch"`
		Leader  bool   `json:"leader"`
		Online  bool   `json:"online"`
	} `json:"control_planes"`
	Nodes []struct {
		Name         string   `json:"name"`
		Version      string   `json:"version"`
		Arch         string   `json:"arch"`
		Roles        []string `json:"roles"`
		Status       string   `json:"status"`
		Pinned       bool     `json:"pinned"`
		ControlPlane string   `json:"control_plane"`
	} `json:"nodes"`
	Rollout *updateRollout `json:"rollout"`
}

// cmdUpdate: `paas update [--check] [--yes] [--version vX]`, `paas update status`,
// `paas update pause|resume|abort` (platform administrators).
func cmdUpdate(args []string) error {
	pos, flags := splitFlags(args)
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "look for a new release now")
	yes := fs.Bool("yes", false, "start the rolling update")
	version := fs.String("version", "", "release to install (default: the latest)")
	_ = fs.Parse(flags)
	if len(pos) > 1 {
		return errors.New("usage: paas update [--check] [--yes] [--version vX] | paas update status | paas update pause|resume|abort")
	}
	if len(pos) == 1 {
		switch pos[0] {
		case "status":
			return updateFollow(false)
		case "pause", "resume", "abort":
			var r updateRollout
			if err := call(http.MethodPost, "/admin/update/"+pos[0], nil, &r); err != nil {
				return err
			}
			fmt.Printf("Update to %s: %s\n", r.Version, r.Status)
			return nil
		default:
			return fmt.Errorf("unknown update command %q", pos[0])
		}
	}
	var info updateInfo
	path, method := "/admin/update", http.MethodGet
	if *check {
		path, method = "/admin/update/check", http.MethodPost
	}
	if err := call(method, path, nil, &info); err != nil {
		return err
	}
	if !*yes {
		printUpdateInfo(&info)
		if info.Rollout != nil && (info.Rollout.Status == "running" || info.Rollout.Status == "paused") {
			fmt.Println("\nAn update is in progress: paas update status")
		} else if info.UpdateAvailable || *version != "" {
			fmt.Println("\nEvery control plane and node is restarted, one at a time, after a backup of the platform state.")
			fmt.Println("Run again with --yes to start.")
		}
		return nil
	}
	target := *version
	if target == "" && info.Latest != nil {
		target = info.Latest.Tag
	}
	if target == "" {
		return errors.New("no release known: run paas update --check, or pass --version")
	}
	var r updateRollout
	if err := call(http.MethodPost, "/admin/update", map[string]any{"version": *version, "confirm": true}, &r); err != nil {
		return err
	}
	fmt.Printf("Updating to %s (Ctrl-C stops watching, not the update)\n", r.Version)
	return updateFollow(true)
}

func printUpdateInfo(info *updateInfo) {
	cur := info.Version
	if info.Development {
		cur += " (development build)"
	}
	fmt.Printf("This control plane: %s\n", cur)
	switch {
	case info.Latest != nil:
		fmt.Printf("Latest release:     %s (%s)", info.Latest.Tag, info.Latest.PublishedAt.Local().Format("2006-01-02"))
		if info.UpdateAvailable {
			fmt.Print("  update available")
		}
		fmt.Println()
	case !info.CheckEnabled:
		fmt.Println("Latest release:     unknown (PAAS_UPDATE_CHECK=false; paas update --check looks now)")
	default:
		fmt.Println("Latest release:     not checked yet (paas update --check)")
	}
	if info.CheckError != "" {
		fmt.Printf("Last check failed:  %s\n", info.CheckError)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "\nMACHINE\tKIND\tVERSION\tARCH\tSTATUS")
	for _, cp := range info.ControlPlanes {
		st := "offline"
		if cp.Online {
			st = "online"
		}
		if cp.Leader {
			st += ", leader"
		}
		fmt.Fprintf(w, "%s\tcontrol plane\t%s\t%s\t%s\n", cp.Name, orDash(cp.Version), orDash(cp.Arch), st)
	}
	for _, n := range info.Nodes {
		if n.ControlPlane != "" {
			continue
		}
		st := n.Status
		if n.Pinned {
			st += ", binary pinned"
		}
		fmt.Fprintf(w, "%s\tnode (%s)\t%s\t%s\t%s\n", n.Name, strings.Join(n.Roles, ","), orDash(n.Version), orDash(n.Arch), st)
	}
	w.Flush()
	if info.Latest != nil && info.UpdateAvailable && info.Latest.Notes != "" {
		fmt.Printf("\nRelease notes (%s):\n%s\n", info.Latest.URL, indent(info.Latest.Notes))
	}
}

// updateFollow prints the rollout; with follow, until it stops.
func updateFollow(follow bool) error {
	printed := map[string]string{}
	for {
		var info updateInfo
		if err := call(http.MethodGet, "/admin/update", nil, &info); err != nil {
			return err
		}
		r := info.Rollout
		if r == nil {
			fmt.Println("No update has been started.")
			return nil
		}
		if !follow {
			fmt.Printf("Update to %s: %s (started %s by %s)\n", r.Version, r.Status, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.RequestedBy)
			if r.Message != "" {
				fmt.Println(r.Message)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "STEP\tSTATUS\tMESSAGE")
			for _, st := range r.Steps {
				fmt.Fprintf(w, "%s\t%s\t%s\n", stepName(st), st.Status, st.Message)
			}
			return w.Flush()
		}
		for _, st := range r.Steps {
			key := st.Status + "|" + st.Message
			if printed[st.Kind+st.Name] != key && st.Status != "pending" {
				printed[st.Kind+st.Name] = key
				line := fmt.Sprintf("  %-32s %s", stepName(st), st.Status)
				if st.Message != "" {
					line += ": " + st.Message
				}
				fmt.Println(line)
			}
		}
		switch r.Status {
		case "done":
			fmt.Printf("Every machine runs %s.\n", r.Version)
			return nil
		case "failed", "aborted":
			return fmt.Errorf("update %s: %s", r.Status, r.Message)
		}
		time.Sleep(3 * time.Second)
	}
}

func stepName(st updateStep) string {
	switch st.Kind {
	case "control_plane":
		return "control plane " + st.Name
	case "node":
		return "node " + st.Name
	}
	return st.Name
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n  ")
}
