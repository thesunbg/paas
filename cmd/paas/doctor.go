package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

// paas doctor: the first-run setup checklist (the dashboard's setup wizard), for use over SSH.

type doctorRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type doctorStep struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	HowTo     string         `json:"howto"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Details   []string       `json:"details"`
	Skipped   bool           `json:"skipped"`
	Completed bool           `json:"completed"`
	Records   []doctorRecord `json:"records"`
}

type doctorReport struct {
	Steps []doctorStep `json:"steps"`
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	refresh := fs.Bool("refresh", false, "run every check again instead of using results from the last 30 seconds")
	verbose := fs.Bool("v", false, "also print how to fix each step")
	_ = fs.Parse(args)
	path := "/admin/setup-checklist"
	if *refresh {
		path += "?refresh=1"
	}
	var rep doctorReport
	if err := call(http.MethodGet, path, nil, &rep); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
			return errors.New("paas doctor needs an administrator's token (paas login as the instance admin)")
		}
		return err
	}
	if failed := printDoctor(os.Stdout, cfg.Server, rep, *verbose); failed > 0 {
		os.Exit(1)
	}
	return nil
}

// printDoctor writes the checklist and returns how many steps failed (skipped ones not counted).
func printDoctor(w io.Writer, server string, rep doctorReport, verbose bool) int {
	fmt.Fprintf(w, "Setup checklist for %s\n\n", server)
	marks := map[string]string{"ok": "[ ok ]", "warn": "[warn]", "fail": "[FAIL]"}
	var ok, warn, failed int
	for i, st := range rep.Steps {
		mark := marks[st.Status]
		if mark == "" {
			mark = "[ ?? ]"
		}
		suffix := ""
		if st.Skipped {
			suffix = " (skipped)"
		}
		fmt.Fprintf(w, "%s %d. %s%s\n", mark, i+1, st.Title, suffix)
		fmt.Fprintf(w, "       %s\n", st.Summary)
		for _, d := range st.Details {
			fmt.Fprintf(w, "       - %s\n", d)
		}
		if len(st.Records) > 0 {
			fmt.Fprintln(w, "       Create these DNS records:")
			for _, r := range st.Records {
				fmt.Fprintf(w, "         %-32s %-4s %s\n", r.Name, r.Type, r.Value)
			}
		}
		bad := st.Status == "fail" && !st.Skipped
		if st.HowTo != "" && (verbose || bad) {
			fmt.Fprintf(w, "       How to: %s\n", st.HowTo)
		}
		switch {
		case bad:
			failed++
		case st.Status == "ok":
			ok++
		case st.Status == "warn":
			warn++
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "%d ok, %d warnings, %d failed. Fix them step by step in the dashboard: Settings → System → Setup checklist.\n", ok, warn, failed)
	return failed
}
