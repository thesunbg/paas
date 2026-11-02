// Command paas is the command-line client for the platform API.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

type cliConfig struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	Team    string `json:"team,omitempty"`
	Project string `json:"project,omitempty"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "paas", "config.json")
}

func loadConfig() cliConfig {
	var c cliConfig
	if data, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(data, &c)
	}
	if v := os.Getenv("PAAS_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("PAAS_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("PAAS_PROJECT"); v != "" {
		c.Project = v
	}
	if c.Server == "" {
		c.Server = "http://127.0.0.1:8080"
	}
	return c
}

func saveConfig(c cliConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

var cfg cliConfig

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status) }

func call(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(cfg.Server, "/")+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// stream reads a Server-Sent Events endpoint, printing `log` events. Returns the `done` payload.
func stream(path string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(cfg.Server, "/")+"/api/v1"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("stream: HTTP %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if event == "done" {
				return data, nil
			}
			fmt.Println(data)
		}
	}
	return "", sc.Err()
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func usage() {
	fmt.Print(`paas — deploy apps from git in seconds

Usage:
  paas login [--server URL]            Log in and store a token
  paas logout | whoami
  paas use <project>                   Set the default project
  paas projects                        List projects
  paas projects create <name>
  paas ls                              List services and databases of the project
  paas create <name> --repo URL [--branch main] [--type web|worker|static] [--size small] [--sleep] [--root DIR]
  paas create <name> --image IMAGE [--port 8080]
  paas deploy <service> [--commit SHA] [--no-follow]
  paas status <service>
  paas logs <service> [-f] [--tail 200]
  paas env <service>                   Show environment variables
  paas env set <service> K=V [K=V...]  Set variables (rolling restart)
  paas env unset <service> K
  paas scale <service> <replicas>
  paas restart|stop|start <service>
  paas rollback <service> [deployment-id]
  paas exec <service> -- <command>
  paas domains <service> [add|rm <hostname>]
  paas scan <service> [--all]          Latest vulnerability scan of the service's image
  paas scan policy [off|warn|block] [--threshold CRITICAL|HIGH|MEDIUM|LOW]
                                       Show or set the project's scan policy (setting: team admin)
  paas db ls
  paas db create <name> --engine postgres|mysql|mariadb|redis|mongo [--mode shared|dedicated] [--size micro] [--link svc]
  paas db info <name>
  paas db backup <name>
  paas db link <name> <service> [--env DATABASE_URL]
  paas run <cron-service>              Run a cron job now and show its output
  paas runs <cron-service>             Recent job runs
  paas templates                       List one-click templates
  paas install <template> [--domain D] Install a template into the project
  paas compose <docker-compose.yml>    Import a compose file into the project
  paas nodes                           Cluster nodes and density (admin)
  paas drain <node>                    Move workloads off a node (admin)
  paas join-command [--roles r1,r2]    Print a single-use command to add a node (admin; roles default to worker)

Global flags via env: PAAS_SERVER, PAAS_TOKEN, PAAS_PROJECT.
`)
}

func main() {
	cfg = loadConfig()
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage()
		return
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(rest)
	case "logout":
		_ = call(http.MethodPost, "/auth/logout", nil, nil)
		cfg.Token = ""
		err = saveConfig(cfg)
	case "whoami":
		err = cmdWhoami()
	case "use":
		err = cmdUse(rest)
	case "projects":
		err = cmdProjects(rest)
	case "ls", "services":
		err = cmdList()
	case "create":
		err = cmdCreate(rest)
	case "deploy":
		err = cmdDeploy(rest)
	case "status":
		err = cmdStatus(rest)
	case "logs":
		err = cmdLogs(rest)
	case "env":
		err = cmdEnv(rest)
	case "scale":
		err = cmdScale(rest)
	case "restart", "stop", "start":
		err = cmdAction(cmd, rest)
	case "rollback":
		err = cmdRollback(rest)
	case "exec":
		err = cmdExec(rest)
	case "domains":
		err = cmdDomains(rest)
	case "scan":
		err = cmdScan(rest)
	case "db":
		err = cmdDB(rest)
	case "nodes":
		err = cmdNodes()
	case "run":
		err = cmdRun(rest)
	case "runs":
		err = cmdRuns(rest)
	case "templates":
		err = cmdTemplates()
	case "install":
		err = cmdInstall(rest)
	case "compose":
		err = cmdCompose(rest)
	case "drain":
		err = cmdDrain(rest)
	case "join-command":
		err = cmdJoinCommand(rest)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		die(err)
	}
}

func cmdJoinCommand(args []string) error {
	roles := []string{"worker"}
	for i := 0; i < len(args); i++ {
		if args[i] != "--roles" || i+1 >= len(args) {
			return fmt.Errorf("usage: paas join-command [--roles r1,r2]")
		}
		i++
		roles = strings.Split(args[i], ",")
	}
	var out struct {
		Command   string `json:"command"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := call(http.MethodPost, "/cluster/join-tokens", map[string]any{"roles": roles}, &out); err != nil {
		return err
	}
	fmt.Println(out.Command)
	fmt.Fprintln(os.Stderr, "single-use; expires", out.ExpiresAt)
	return nil
}

func prompt(label string, secret bool) string {
	fmt.Fprint(os.Stderr, label)
	if secret && term.IsTerminal(int(os.Stdin.Fd())) {
		b, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return string(b)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line)
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	server := fs.String("server", cfg.Server, "control plane URL")
	email := fs.String("email", "", "email")
	password := fs.String("password", os.Getenv("PAAS_PASSWORD"), "password")
	_ = fs.Parse(args)
	cfg.Server = strings.TrimRight(*server, "/")
	if *email == "" {
		*email = prompt("Email: ", false)
	}
	if *password == "" {
		*password = prompt("Password: ", true)
	}
	var setup struct {
		NeedsSetup bool `json:"needs_setup"`
	}
	if err := call(http.MethodGet, "/setup", nil, &setup); err != nil {
		return err
	}
	path := "/auth/login"
	if setup.NeedsSetup {
		fmt.Fprintln(os.Stderr, "No users yet: creating the administrator account.")
		path = "/auth/register"
	}
	var out struct {
		Token string `json:"token"`
		User  struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	cfg.Token = ""
	if err := call(http.MethodPost, path, map[string]string{"email": *email, "password": *password}, &out); err != nil {
		return err
	}
	cfg.Token = out.Token
	var me meResp
	if err := call(http.MethodGet, "/me", nil, &me); err == nil && len(me.Teams) > 0 {
		cfg.Team = me.Teams[0].ID
	}
	fmt.Println("Logged in as", out.User.Email)
	return saveConfig(cfg)
}

type meResp struct {
	User struct {
		Email   string `json:"email"`
		IsAdmin bool   `json:"is_admin"`
	} `json:"user"`
	Teams []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"teams"`
}

func cmdWhoami() error {
	var me meResp
	if err := call(http.MethodGet, "/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("%s (admin=%v) on %s\n", me.User.Email, me.User.IsAdmin, cfg.Server)
	for _, t := range me.Teams {
		mark := " "
		if t.ID == cfg.Team {
			mark = "*"
		}
		fmt.Printf(" %s team %s (%s)\n", mark, t.Name, t.Role)
	}
	return nil
}

type project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	TeamID    string `json:"team_id"`
	Running   int    `json:"running"`
	Services  int    `json:"services"`
	Databases int    `json:"databases"`
}

func teamID() (string, error) {
	if cfg.Team != "" {
		return cfg.Team, nil
	}
	var me meResp
	if err := call(http.MethodGet, "/me", nil, &me); err != nil {
		return "", err
	}
	if len(me.Teams) == 0 {
		return "", errors.New("you are not a member of any team")
	}
	cfg.Team = me.Teams[0].ID
	return cfg.Team, nil
}

func listProjects() ([]project, error) {
	tid, err := teamID()
	if err != nil {
		return nil, err
	}
	var ps []project
	err = call(http.MethodGet, "/teams/"+tid+"/projects", nil, &ps)
	return ps, err
}

func findProject(ref string) (*project, error) {
	ps, err := listProjects()
	if err != nil {
		return nil, err
	}
	for i, p := range ps {
		if p.ID == ref || p.Slug == ref || strings.EqualFold(p.Name, ref) {
			return &ps[i], nil
		}
	}
	return nil, fmt.Errorf("project %q not found", ref)
}

func currentProject() (*project, error) {
	if cfg.Project == "" {
		ps, err := listProjects()
		if err != nil {
			return nil, err
		}
		if len(ps) == 1 {
			return &ps[0], nil
		}
		return nil, errors.New("no project selected: run `paas use <project>` or set PAAS_PROJECT")
	}
	return findProject(cfg.Project)
}

func cmdUse(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas use <project>")
	}
	p, err := findProject(args[0])
	if err != nil {
		return err
	}
	cfg.Project = p.Slug
	fmt.Println("Using project", p.Name)
	return saveConfig(cfg)
}

func cmdProjects(args []string) error {
	if len(args) >= 2 && args[0] == "create" {
		tid, err := teamID()
		if err != nil {
			return err
		}
		var p project
		if err := call(http.MethodPost, "/teams/"+tid+"/projects", map[string]string{"name": strings.Join(args[1:], " ")}, &p); err != nil {
			return err
		}
		fmt.Printf("Created project %s (%s)\n", p.Name, p.Slug)
		if cfg.Project == "" {
			cfg.Project = p.Slug
			_ = saveConfig(cfg)
		}
		return nil
	}
	ps, err := listProjects()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tSLUG\tSERVICES\tRUNNING\tDATABASES")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", p.Name, p.Slug, p.Services, p.Running, p.Databases)
	}
	return tw.Flush()
}

type service struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	Slug                string  `json:"slug"`
	Type                string  `json:"type"`
	Status              string  `json:"status"`
	URL                 string  `json:"url"`
	Instances           int     `json:"instances"`
	MemoryMB            int     `json:"memory_mb"`
	CurrentDeploymentID *string `json:"current_deployment_id"`
}

type database struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Engine     string `json:"engine"`
	Mode       string `json:"mode"`
	SizePreset string `json:"size_preset"`
	Status     string `json:"status"`
}

type projectDetail struct {
	Project   project    `json:"project"`
	Services  []service  `json:"services"`
	Databases []database `json:"databases"`
}

func loadProject() (*projectDetail, error) {
	p, err := currentProject()
	if err != nil {
		return nil, err
	}
	var d projectDetail
	err = call(http.MethodGet, "/projects/"+p.ID, nil, &d)
	return &d, err
}

func findService(ref string) (*service, *projectDetail, error) {
	d, err := loadProject()
	if err != nil {
		return nil, nil, err
	}
	for i, s := range d.Services {
		if s.ID == ref || s.Slug == ref || strings.EqualFold(s.Name, ref) {
			return &d.Services[i], d, nil
		}
	}
	return nil, nil, fmt.Errorf("service %q not found in project %s", ref, d.Project.Name)
}

func findDatabase(ref string) (*database, error) {
	d, err := loadProject()
	if err != nil {
		return nil, err
	}
	for i, db := range d.Databases {
		if db.ID == ref || db.Name == ref {
			return &d.Databases[i], nil
		}
	}
	return nil, fmt.Errorf("database %q not found", ref)
}

func cmdList() error {
	d, err := loadProject()
	if err != nil {
		return err
	}
	fmt.Printf("Project %s\n\n", d.Project.Name)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tTYPE\tSTATUS\tINSTANCES\tMEMORY\tURL")
	for _, s := range d.Services {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d MB\t%s\n", s.Slug, s.Type, s.Status, s.Instances, s.MemoryMB, s.URL)
	}
	_ = tw.Flush()
	if len(d.Databases) > 0 {
		fmt.Println()
		tw = tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "DATABASE\tENGINE\tMODE\tSIZE\tSTATUS")
		for _, db := range d.Databases {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", db.Name, db.Engine, db.Mode, db.SizePreset, db.Status)
		}
		_ = tw.Flush()
	}
	return nil
}

// splitFlags lets flags appear after positional arguments.
func splitFlags(args []string) (pos, flags []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !boolFlag(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return
}

func boolFlag(f string) bool {
	switch strings.TrimLeft(f, "-") {
	case "sleep", "f", "follow", "no-follow", "no-deploy", "no-restart", "all":
		return true
	}
	return false
}

func cmdCreate(args []string) error {
	pos, flags := splitFlags(args)
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	repo := fs.String("repo", "", "git repository URL")
	branch := fs.String("branch", "main", "branch")
	root := fs.String("root", "", "root directory in the repository")
	image := fs.String("image", "", "deploy an existing image instead of building")
	typ := fs.String("type", "web", "web | worker | static")
	size := fs.String("size", "small", "instance size")
	port := fs.Int("port", 0, "port the app listens on")
	sleep := fs.Bool("sleep", false, "sleep when idle")
	token := fs.String("git-token", "", "token for private repositories")
	domain := fs.String("domain", "", "custom domain")
	noFollow := fs.Bool("no-follow", false, "do not stream the first deployment")
	_ = fs.Parse(flags)
	if len(pos) != 1 || (*repo == "" && *image == "") {
		return errors.New("usage: paas create <name> --repo URL | --image IMAGE")
	}
	p, err := currentProject()
	if err != nil {
		return err
	}
	body := map[string]any{"name": pos[0], "type": *typ, "size": *size}
	if *image != "" {
		body["source"] = map[string]any{"kind": "image", "image": *image}
	} else {
		body["source"] = map[string]any{"kind": "git", "repo": *repo, "branch": *branch, "root_dir": *root}
	}
	if *port > 0 {
		body["port"] = *port
	}
	if *sleep {
		body["sleep"] = map[string]any{"enabled": true, "idle_minutes": 15}
	}
	if *token != "" {
		body["git_token"] = *token
	}
	if *domain != "" {
		body["domain"] = *domain
	}
	var out struct {
		Service    service `json:"service"`
		Deployment *struct {
			ID string `json:"id"`
		} `json:"deployment"`
	}
	if err := call(http.MethodPost, "/projects/"+p.ID+"/services", body, &out); err != nil {
		return err
	}
	fmt.Printf("Created service %s\n", out.Service.Slug)
	if out.Deployment != nil && !*noFollow {
		return followDeployment(out.Deployment.ID)
	}
	return nil
}

func followDeployment(id string) error {
	start := time.Now()
	status, err := stream("/deployments/" + id + "/logs/stream")
	if err != nil {
		return err
	}
	if status != "active" {
		return fmt.Errorf("deployment %s", status)
	}
	fmt.Printf("\n✔ deployed in %s\n", time.Since(start).Round(100*time.Millisecond))
	return nil
}

func cmdDeploy(args []string) error {
	pos, flags := splitFlags(args)
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	commit := fs.String("commit", "", "commit sha")
	noFollow := fs.Bool("no-follow", false, "do not stream logs")
	_ = fs.Parse(flags)
	if len(pos) != 1 {
		return errors.New("usage: paas deploy <service>")
	}
	s, _, err := findService(pos[0])
	if err != nil {
		return err
	}
	var d struct {
		ID string `json:"id"`
	}
	if err := call(http.MethodPost, "/services/"+s.ID+"/deploy", map[string]string{"commit_sha": *commit}, &d); err != nil {
		return err
	}
	fmt.Println("Deployment", d.ID, "queued")
	if *noFollow {
		return nil
	}
	return followDeployment(d.ID)
}

func cmdStatus(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas status <service>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var d struct {
		Service struct {
			Status string `json:"status"`
			Type   string `json:"type"`
			Spec   struct {
				Replicas  int `json:"replicas"`
				Resources struct {
					MemoryLimitMB   int `json:"memory_limit_mb"`
					MemoryRequestMB int `json:"memory_request_mb"`
				} `json:"resources"`
				QoS string `json:"qos"`
			} `json:"spec"`
		} `json:"service"`
		URLs      []string `json:"urls"`
		Internal  string   `json:"internal_host"`
		Instances []struct {
			ContainerID string `json:"container_id"`
			NodeName    string `json:"node_name"`
			IP          string `json:"ip"`
			Status      string `json:"status"`
			MemoryMB    int    `json:"memory_usage_mb"`
			PeakMB      int    `json:"memory_peak_mb"`
			CPU         int    `json:"cpu_usage_millis"`
			Restarts    int    `json:"restarts"`
		} `json:"instances"`
		Deployments []struct {
			ID        string    `json:"id"`
			Status    string    `json:"status"`
			CommitSHA string    `json:"commit_sha"`
			Message   string    `json:"commit_message"`
			Detected  string    `json:"detected"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"deployments"`
		Recommendation *struct {
			Message string `json:"message"`
		} `json:"recommendation"`
	}
	if err := call(http.MethodGet, "/services/"+s.ID, nil, &d); err != nil {
		return err
	}
	fmt.Printf("%s  [%s]  %s  replicas=%d  memory=%d/%d MB (%s)\n", s.Slug, d.Service.Type, d.Service.Status, d.Service.Spec.Replicas,
		d.Service.Spec.Resources.MemoryRequestMB, d.Service.Spec.Resources.MemoryLimitMB, d.Service.Spec.QoS)
	for _, u := range d.URLs {
		fmt.Println("  url:", u)
	}
	fmt.Println("  internal:", d.Internal)
	if len(d.Instances) > 0 {
		fmt.Println()
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "INSTANCE\tNODE\tIP\tSTATUS\tMEM\tPEAK\tCPU\tRESTARTS")
		for _, in := range d.Instances {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d MB\t%d MB\t%dm\t%d\n", in.ContainerID, in.NodeName, in.IP, in.Status, in.MemoryMB, in.PeakMB, in.CPU, in.Restarts)
		}
		_ = tw.Flush()
	}
	if len(d.Deployments) > 0 {
		fmt.Println()
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "DEPLOYMENT\tSTATUS\tCOMMIT\tDETECTED\tWHEN")
		for _, dep := range d.Deployments {
			sha := dep.CommitSHA
			if len(sha) > 7 {
				sha = sha[:7]
			}
			fmt.Fprintf(tw, "%s\t%s\t%s %s\t%s\t%s ago\n", dep.ID[:8], dep.Status, sha, truncate(dep.Message, 40), dep.Detected, time.Since(dep.CreatedAt).Round(time.Second))
		}
		_ = tw.Flush()
	}
	if d.Recommendation != nil {
		fmt.Println("\n💡", d.Recommendation.Message)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func cmdLogs(args []string) error {
	pos, flags := splitFlags(args)
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	tail := fs.Int("tail", 200, "lines")
	_ = fs.Parse(flags)
	if len(pos) != 1 {
		return errors.New("usage: paas logs <service> [-f]")
	}
	s, _, err := findService(pos[0])
	if err != nil {
		return err
	}
	if *follow {
		_, err := stream("/services/" + s.ID + "/logs/stream")
		return err
	}
	var out []struct {
		Instance string   `json:"instance"`
		Lines    []string `json:"lines"`
		Error    string   `json:"error"`
	}
	if err := call(http.MethodGet, fmt.Sprintf("/services/%s/logs?tail=%d", s.ID, *tail), nil, &out); err != nil {
		return err
	}
	for _, in := range out {
		for _, l := range in.Lines {
			if len(out) > 1 {
				fmt.Printf("%s | %s\n", in.Instance, l)
			} else {
				fmt.Println(l)
			}
		}
		if in.Error != "" {
			fmt.Fprintln(os.Stderr, in.Instance+":", in.Error)
		}
	}
	return nil
}

func cmdEnv(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: paas env <service> | paas env set <service> K=V... | paas env unset <service> K")
	}
	switch args[0] {
	case "set":
		if len(args) < 3 {
			return errors.New("usage: paas env set <service> K=V [K=V...]")
		}
		s, _, err := findService(args[1])
		if err != nil {
			return err
		}
		vars := map[string]string{}
		for _, kv := range args[2:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("expected KEY=VALUE, got %q", kv)
			}
			vars[k] = v
		}
		var out map[string]any
		if err := call(http.MethodPut, "/services/"+s.ID+"/env", map[string]any{"vars": vars}, &out); err != nil {
			return err
		}
		fmt.Printf("Saved %d variable(s)", len(vars))
		if _, ok := out["deployment"]; ok {
			fmt.Print("; rolling restart started")
		}
		fmt.Println()
		return nil
	case "unset":
		if len(args) != 3 {
			return errors.New("usage: paas env unset <service> KEY")
		}
		s, _, err := findService(args[1])
		if err != nil {
			return err
		}
		return call(http.MethodDelete, "/services/"+s.ID+"/env/"+url.PathEscape(args[2]), nil, nil)
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var out struct {
		Vars   map[string]string `json:"vars"`
		Linked map[string]string `json:"linked"`
	}
	if err := call(http.MethodGet, "/services/"+s.ID+"/env", nil, &out); err != nil {
		return err
	}
	keys := make([]string, 0, len(out.Vars))
	for k := range out.Vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%s\n", k, out.Vars[k])
	}
	for k, v := range out.Linked {
		fmt.Printf("%s %s\n", k, v)
	}
	return nil
}

func cmdScale(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: paas scale <service> <replicas>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var n int
	if _, err := fmt.Sscan(args[1], &n); err != nil {
		return err
	}
	if err := call(http.MethodPost, "/services/"+s.ID+"/scale", map[string]int{"replicas": n}, nil); err != nil {
		return err
	}
	fmt.Printf("Scaling %s to %d replica(s)\n", s.Slug, n)
	return nil
}

func cmdAction(action string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: paas %s <service>", action)
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var out map[string]any
	if err := call(http.MethodPost, "/services/"+s.ID+"/"+action, nil, &out); err != nil {
		return err
	}
	if id, ok := out["id"].(string); ok && action == "restart" {
		return followDeployment(id)
	}
	fmt.Println(s.Slug, map[string]string{"stop": "stopped", "start": "started", "restart": "restarted"}[action])
	return nil
}

func cmdRollback(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: paas rollback <service> [deployment-id]")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var deps []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Image  string `json:"image"`
		Static string `json:"static_path"`
	}
	if err := call(http.MethodGet, "/services/"+s.ID+"/deployments", nil, &deps); err != nil {
		return err
	}
	target := ""
	if len(args) == 2 {
		for _, d := range deps {
			if strings.HasPrefix(d.ID, args[1]) {
				target = d.ID
			}
		}
	} else {
		seenActive := false
		for _, d := range deps {
			if d.Status == "active" {
				seenActive = true
				continue
			}
			if seenActive && d.Status == "superseded" && (d.Image != "" || d.Static != "") {
				target = d.ID
				break
			}
		}
	}
	if target == "" {
		return errors.New("no previous deployment to roll back to")
	}
	var d struct {
		ID string `json:"id"`
	}
	if err := call(http.MethodPost, "/deployments/"+target+"/rollback", nil, &d); err != nil {
		return err
	}
	fmt.Println("Rolling back to", target[:8])
	return followDeployment(d.ID)
}

func cmdExec(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: paas exec <service> -- <command>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	rest := args[1:]
	if rest[0] == "--" {
		rest = rest[1:]
	}
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Error    string `json:"error"`
	}
	if err := call(http.MethodPost, "/services/"+s.ID+"/exec", map[string]string{"command": strings.Join(rest, " ")}, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	fmt.Print(out.Stdout)
	fmt.Fprint(os.Stderr, out.Stderr)
	if out.ExitCode != 0 {
		os.Exit(out.ExitCode)
	}
	return nil
}

func cmdDomains(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: paas domains <service> [add|rm <hostname>]")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	type domain struct {
		ID          string `json:"id"`
		Hostname    string `json:"hostname"`
		IsGenerated bool   `json:"is_generated"`
	}
	if len(args) == 3 && args[1] == "add" {
		var d domain
		if err := call(http.MethodPost, "/services/"+s.ID+"/domains", map[string]string{"hostname": args[2]}, &d); err != nil {
			return err
		}
		fmt.Printf("Added %s — point its DNS A record at your edge node(s).\n", d.Hostname)
		return nil
	}
	var ds []domain
	if err := call(http.MethodGet, "/services/"+s.ID+"/domains", nil, &ds); err != nil {
		return err
	}
	if len(args) == 3 && args[1] == "rm" {
		for _, d := range ds {
			if d.Hostname == args[2] {
				return call(http.MethodDelete, "/services/"+s.ID+"/domains/"+d.ID, nil, nil)
			}
		}
		return fmt.Errorf("domain %s not found", args[2])
	}
	for _, d := range ds {
		suffix := ""
		if d.IsGenerated {
			suffix = " (generated)"
		}
		fmt.Println(d.Hostname + suffix)
	}
	return nil
}

func cmdDB(args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	switch args[0] {
	case "ls":
		return cmdList()
	case "create":
		pos, flags := splitFlags(args[1:])
		fs := flag.NewFlagSet("db create", flag.ExitOnError)
		engine := fs.String("engine", "postgres", "postgres | mysql | mariadb | redis | mongo")
		mode := fs.String("mode", "", "shared | dedicated")
		size := fs.String("size", "micro", "size preset")
		link := fs.String("link", "", "comma separated services to link")
		_ = fs.Parse(flags)
		if len(pos) != 1 {
			return errors.New("usage: paas db create <name> --engine postgres")
		}
		d, err := loadProject()
		if err != nil {
			return err
		}
		var links []string
		for _, l := range strings.Split(*link, ",") {
			if l = strings.TrimSpace(l); l == "" {
				continue
			}
			for _, s := range d.Services {
				if s.Slug == l || s.ID == l {
					links = append(links, s.ID)
				}
			}
		}
		var db database
		if err := call(http.MethodPost, "/projects/"+d.Project.ID+"/databases", map[string]any{"name": pos[0], "engine": *engine, "mode": *mode, "size": *size, "link_services": links}, &db); err != nil {
			return err
		}
		fmt.Printf("Provisioning %s %s database %q", db.Mode, db.Engine, db.Name)
		for i := 0; i < 300; i++ {
			var info struct {
				Database database `json:"database"`
			}
			if err := call(http.MethodGet, "/databases/"+db.ID, nil, &info); err != nil {
				return err
			}
			if info.Database.Status != "provisioning" {
				fmt.Println(" →", info.Database.Status)
				return nil
			}
			fmt.Print(".")
			time.Sleep(2 * time.Second)
		}
		fmt.Println()
		return nil
	case "info":
		if len(args) != 2 {
			return errors.New("usage: paas db info <name>")
		}
		db, err := findDatabase(args[1])
		if err != nil {
			return err
		}
		var info struct {
			Database   database       `json:"database"`
			Connection map[string]any `json:"connection"`
		}
		if err := call(http.MethodGet, "/databases/"+db.ID, nil, &info); err != nil {
			return err
		}
		fmt.Printf("%s (%s, %s, %s): %s\n", info.Database.Name, info.Database.Engine, info.Database.Mode, info.Database.SizePreset, info.Database.Status)
		if u, ok := info.Connection["url"]; ok {
			fmt.Println("url:", u)
		}
		return nil
	case "backup":
		if len(args) != 2 {
			return errors.New("usage: paas db backup <name>")
		}
		db, err := findDatabase(args[1])
		if err != nil {
			return err
		}
		var b struct {
			Status string `json:"status"`
			Size   int64  `json:"size_bytes"`
			Path   string `json:"path"`
		}
		if err := call(http.MethodPost, "/databases/"+db.ID+"/backups", nil, &b); err != nil {
			return err
		}
		fmt.Printf("Backup %s: %d bytes (%s)\n", b.Status, b.Size, b.Path)
		return nil
	case "link":
		pos, flags := splitFlags(args[1:])
		fs := flag.NewFlagSet("db link", flag.ExitOnError)
		envName := fs.String("env", "", "environment variable name")
		_ = fs.Parse(flags)
		if len(pos) != 2 {
			return errors.New("usage: paas db link <database> <service> [--env NAME]")
		}
		db, err := findDatabase(pos[0])
		if err != nil {
			return err
		}
		s, _, err := findService(pos[1])
		if err != nil {
			return err
		}
		var out map[string]string
		if err := call(http.MethodPost, "/databases/"+db.ID+"/links", map[string]string{"service_id": s.ID, "env_name": *envName}, &out); err != nil {
			return err
		}
		fmt.Printf("Linked %s → %s as %s\n", db.Name, s.Slug, out["env_name"])
		return nil
	}
	return errors.New("unknown db command")
}

func cmdNodes() error {
	var out struct {
		Nodes []struct {
			Name      string   `json:"name"`
			Address   string   `json:"address"`
			Roles     []string `json:"roles"`
			Status    string   `json:"status"`
			CPUCores  int      `json:"cpu_cores"`
			MemoryMB  int      `json:"memory_mb"`
			Committed int      `json:"committed_memory_mb"`
			Capacity  int      `json:"capacity_memory_mb"`
			UsedApps  int      `json:"used_by_apps_mb"`
			Instances int      `json:"instances"`
			Ratio     float64  `json:"overcommit_ratio"`
			Usage     struct {
				CPUPercent float64 `json:"cpu_percent"`
				MemAvail   int     `json:"memory_available_mb"`
			} `json:"usage"`
		} `json:"nodes"`
		Sleeping int `json:"sleeping_services"`
	}
	if err := call(http.MethodGet, "/nodes", nil, &out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tADDRESS\tROLES\tSTATUS\tCPU\tMEMORY\tCOMMITTED/CAPACITY\tAPPS USE\tINSTANCES\tOVERCOMMIT")
	for _, n := range out.Nodes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d (%.0f%%)\t%d MB (%d free)\t%d/%d MB\t%d MB\t%d\t%.2fx\n", n.Name, n.Address, strings.Join(n.Roles, ","), n.Status,
			n.CPUCores, n.Usage.CPUPercent, n.MemoryMB, n.Usage.MemAvail, n.Committed, n.Capacity, n.UsedApps, n.Instances, n.Ratio)
	}
	_ = tw.Flush()
	fmt.Printf("\nsleeping services: %d\n", out.Sleeping)
	return nil
}

type jobRun struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	ExitCode   *int       `json:"exit_code"`
	Output     string     `json:"output"`
	Trigger    string     `json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func cmdRun(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas run <cron-service>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var run jobRun
	if err := call(http.MethodPost, "/services/"+s.ID+"/run", nil, &run); err != nil {
		return err
	}
	fmt.Println("Job started:", run.ID[:8])
	for i := 0; i < 1800; i++ {
		var runs []jobRun
		if err := call(http.MethodGet, "/services/"+s.ID+"/runs", nil, &runs); err != nil {
			return err
		}
		for _, r := range runs {
			if r.ID == run.ID && r.Status != "running" {
				fmt.Println(r.Output)
				code := -1
				if r.ExitCode != nil {
					code = *r.ExitCode
				}
				fmt.Printf("\n%s (exit %d)\n", r.Status, code)
				if r.Status != "succeeded" {
					os.Exit(1)
				}
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return errors.New("timed out waiting for the job")
}

func cmdRuns(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas runs <cron-service>")
	}
	s, _, err := findService(args[0])
	if err != nil {
		return err
	}
	var runs []jobRun
	if err := call(http.MethodGet, "/services/"+s.ID+"/runs", nil, &runs); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tTRIGGER\tSTATUS\tEXIT\tSTARTED\tDURATION")
	for _, r := range runs {
		exit, dur := "-", "-"
		if r.ExitCode != nil {
			exit = fmt.Sprint(*r.ExitCode)
		}
		if r.FinishedAt != nil {
			dur = r.FinishedAt.Sub(r.StartedAt).Round(100 * time.Millisecond).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID[:8], r.Trigger, r.Status, exit, r.StartedAt.Local().Format("01-02 15:04:05"), dur)
	}
	return tw.Flush()
}

func cmdTemplates() error {
	var list []struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Category    string `json:"category"`
		Description string `json:"description"`
	}
	if err := call(http.MethodGet, "/templates", nil, &list); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "TEMPLATE\tCATEGORY\tDESCRIPTION")
	for _, t := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.ID, t.Category, t.Description)
	}
	return tw.Flush()
}

func cmdInstall(args []string) error {
	pos, flags := splitFlags(args)
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	domain := fs.String("domain", "", "custom domain for the main service")
	_ = fs.Parse(flags)
	if len(pos) != 1 {
		return errors.New("usage: paas install <template> [--domain D]")
	}
	p, err := currentProject()
	if err != nil {
		return err
	}
	var out struct {
		Services []service `json:"services"`
	}
	if err := call(http.MethodPost, "/projects/"+p.ID+"/templates/"+pos[0], map[string]string{"domain": *domain}, &out); err != nil {
		return err
	}
	for _, s := range out.Services {
		fmt.Println("Created", s.Slug, "- deploying once its databases are ready")
	}
	return nil
}

func cmdCompose(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas compose <docker-compose.yml>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	p, err := currentProject()
	if err != nil {
		return err
	}
	var out struct {
		Services []service `json:"services"`
		Warnings []string  `json:"warnings"`
	}
	if err := call(http.MethodPost, "/projects/"+p.ID+"/compose", map[string]string{"compose": string(data)}, &out); err != nil {
		return err
	}
	for _, s := range out.Services {
		fmt.Printf("Created %s (%s)\n", s.Slug, s.Type)
	}
	for _, w := range out.Warnings {
		fmt.Println("warning:", w)
	}
	return nil
}

func cmdDrain(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: paas drain <node>")
	}
	var out struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := call(http.MethodGet, "/nodes", nil, &out); err != nil {
		return err
	}
	for _, n := range out.Nodes {
		if n.Name == args[0] || n.ID == args[0] {
			var res map[string]any
			if err := call(http.MethodPost, "/nodes/"+n.ID+"/drain", nil, &res); err != nil {
				return err
			}
			fmt.Printf("Draining %s: moving %v instance(s); stateful services kept: %v\n", n.Name, res["moving"], res["pinned_stateful"])
			return nil
		}
	}
	return fmt.Errorf("node %q not found", args[0])
}
