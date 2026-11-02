package main

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
)

type gitCredential struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Host      string `json:"host"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	UpdatedAt string `json:"updated_at"`
}

// cmdGitCredentials manages the team's tokens for GitLab, Gitea/Forgejo, Bitbucket and GitHub.
func cmdGitCredentials(args []string) error {
	tid, err := teamID()
	if err != nil {
		return err
	}
	base := "/teams/" + tid + "/git-credentials"
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "ls", "list":
		var out struct {
			Credentials []gitCredential `json:"credentials"`
		}
		if err := call(http.MethodGet, base, nil, &out); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tPROVIDER\tUSERNAME\tNAME\tID")
		for _, c := range out.Credentials {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.Host, c.Provider, cmp.Or(c.Username, "-"), c.Name, c.ID)
		}
		return tw.Flush()
	case "add", "set":
		pos, flags := splitFlags(args[1:])
		fs := flag.NewFlagSet("git-credentials add", flag.ExitOnError)
		host := fs.String("host", "", "git host, e.g. gitlab.example.com (defaults: gitlab.com, bitbucket.org, github.com)")
		user := fs.String("username", "", "user for clones (Bitbucket app passwords need your username)")
		token := fs.String("token", os.Getenv("PAAS_GIT_TOKEN"), "access token or app password (or PAAS_GIT_TOKEN; prompted when empty)")
		name := fs.String("name", "", "label")
		_ = fs.Parse(flags)
		if len(pos) != 1 {
			return errors.New("usage: paas git-credentials add <gitlab|gitea|bitbucket|github> [--host H] [--username U] [--token T] [--name N]")
		}
		if *token == "" {
			*token = prompt("Token: ", true)
		}
		var c gitCredential
		if err := call(http.MethodPost, base, map[string]string{"provider": pos[0], "host": *host, "username": *user, "token": *token, "name": *name}, &c); err != nil {
			return err
		}
		fmt.Printf("Saved %s credential for %s: private repositories on https://%s/ can now be deployed.\n", c.Provider, c.Host, c.Host)
		return nil
	case "rm", "delete":
		if len(args) != 2 {
			return errors.New("usage: paas git-credentials rm <host|id>")
		}
		var out struct {
			Credentials []gitCredential `json:"credentials"`
		}
		if err := call(http.MethodGet, base, nil, &out); err != nil {
			return err
		}
		for _, c := range out.Credentials {
			if c.ID == args[1] || strings.EqualFold(c.Host, args[1]) {
				if err := call(http.MethodDelete, base+"/"+c.ID, nil, nil); err != nil {
					return err
				}
				fmt.Println("Removed credential for", c.Host)
				return nil
			}
		}
		return fmt.Errorf("no credential for %s", args[1])
	}
	return errors.New("usage: paas git-credentials [ls|add|rm]")
}

// cmdWebhook prints the push-to-deploy webhook of a service for its git host.
func cmdWebhook(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas webhook <service>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var out struct {
		Provider string `json:"provider"`
		URL      string `json:"url"`
		Secret   string `json:"secret"`
	}
	if err := call(http.MethodGet, "/services/"+s.ID+"/webhook", nil, &out); err != nil {
		return err
	}
	fmt.Printf("URL:    %s\nSecret: %s\n\n", out.URL, out.Secret)
	switch out.Provider {
	case "gitlab":
		fmt.Println("GitLab: Settings → Webhooks. Paste the secret as \"Secret token\"; trigger: Push events (and Merge request events for previews).")
	case "gitea":
		fmt.Println("Gitea / Forgejo: Settings → Webhooks → Add webhook → Gitea/Forgejo. Content type application/json; paste the secret; trigger on Push (and Pull request for previews).")
	case "bitbucket":
		fmt.Println("Bitbucket: Repository settings → Webhooks → Add webhook. Paste the secret; triggers: Repository push (and Pull request created/updated/merged/declined for previews).")
	default:
		fmt.Println("GitHub: Settings → Webhooks → Add webhook. Content type application/json; paste the secret; events: Pushes (and Pull requests for previews).")
	}
	return nil
}
