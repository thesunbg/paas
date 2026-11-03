package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"

	"github.com/thesunbg/paas/internal/remote"
)

const defaultInstallerURL = "https://raw.githubusercontent.com/thesunbg/paas/main/internal/installer/install.sh"

// splitArgs separates positional arguments from flags; bools lists the flags that take no value.
func splitArgs(args []string, bools ...string) (pos, flags, rest []string) {
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		for _, b := range bools {
			if name == b {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && !isBool(a) && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return
}

// parseSSHTarget reads user@host[:port] (IPv6 as [addr]:port).
func parseSSHTarget(s string, port int) (user, host string, p int, err error) {
	user, host, ok := strings.Cut(s, "@")
	if !ok {
		user, host = "root", s
	}
	p = port
	if h, ps, err := net.SplitHostPort(host); err == nil {
		host = h
		if p, err = strconv.Atoi(ps); err != nil {
			return "", "", 0, fmt.Errorf("bad port in %q", s)
		}
	}
	host = strings.Trim(host, "[]")
	if user == "" || host == "" {
		return "", "", 0, fmt.Errorf("expected user@host[:port], got %q", s)
	}
	if p <= 0 {
		p = 22
	}
	return user, host, p, nil
}

func knownHostsFiles() []string {
	files := []string{"/etc/ssh/ssh_known_hosts"}
	if home, err := os.UserHomeDir(); err == nil {
		files = append([]string{filepath.Join(home, ".ssh", "known_hosts")}, files...)
	}
	return files
}

// confirmHostKey decides whether to trust key for addr: a --fingerprint that must match,
// else ~/.ssh/known_hosts, else the user confirms interactively (or --yes trusts it on first use).
// It never accepts a key silently: the fingerprint is always printed.
func confirmHostKey(addr string, key ssh.PublicKey, want string, yes bool) error {
	fp := remote.Fingerprint(key)
	fmt.Fprintf(os.Stderr, "%s host key: %s %s\n", addr, key.Type(), fp)
	if want != "" {
		if remote.NormalizeFingerprint(want) != fp {
			return &remote.HostKeyMismatchError{Want: remote.NormalizeFingerprint(want), Got: fp}
		}
		fmt.Fprintln(os.Stderr, "matches --fingerprint")
		return nil
	}
	known, err := remote.CheckKnownHosts(knownHostsFiles(), addr, key)
	if err != nil {
		return err
	}
	if known {
		fmt.Fprintln(os.Stderr, "matches ~/.ssh/known_hosts")
		return nil
	}
	if yes {
		fmt.Fprintln(os.Stderr, "not in known_hosts; trusted on first use because of --yes")
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("unknown host key: compare it with the server's console (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub) and pass --fingerprint " + fp)
	}
	fmt.Fprintln(os.Stderr, "Compare it with the one shown by the provider's console (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub).")
	if a := strings.ToLower(prompt("Is this the server's key? [y/N] ", false)); a != "y" && a != "yes" {
		return errors.New("host key not confirmed")
	}
	return nil
}

func defaultIdentities() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		p := filepath.Join(home, ".ssh", n)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}

// readIdentity reads a private key file, asking for its passphrase when it is encrypted.
func readIdentity(path string) (key []byte, passphrase string, err error) {
	key, err = os.ReadFile(expandHome(path))
	if err != nil {
		return nil, "", err
	}
	if _, err := remote.ParsePrivateKey(key, ""); err != nil {
		if !strings.Contains(err.Error(), "passphrase") {
			return nil, "", fmt.Errorf("%s: %w", path, err)
		}
		passphrase = prompt("Passphrase for "+path+": ", true)
		if _, err := remote.ParsePrivateKey(key, passphrase); err != nil {
			return nil, "", fmt.Errorf("%s: %w", path, err)
		}
	}
	return key, passphrase, nil
}

func cmdNode(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: paas node add <user@host[:port]> ... | paas node installs")
	}
	switch args[0] {
	case "add":
		return cmdNodeAdd(args[1:])
	case "installs":
		return cmdNodeInstalls()
	}
	return fmt.Errorf("unknown node command %q", args[0])
}

// cmdNodeAdd asks the control plane to install a node over SSH. The key or password goes
// to the control plane (over HTTPS), which uses it for this install only.
func cmdNodeAdd(args []string) error {
	pos, flags, _ := splitArgs(args, "password", "yes", "reinstall", "preflight-only", "allow-control-plane", "sudo-password")
	fs := flag.NewFlagSet("node add", flag.ExitOnError)
	identity := fs.String("identity", "", "private key file (default ~/.ssh/id_ed25519, id_ecdsa or id_rsa)")
	usePassword := fs.Bool("password", false, "log in with a password (asked for)")
	sudoPassword := fs.Bool("sudo-password", false, "with a key: ask for the sudo password of a non-root user")
	port := fs.Int("port", 22, "SSH port")
	roles := fs.String("roles", "worker", "worker,build,edge")
	name := fs.String("name", "", "node name (default: the machine's hostname)")
	fingerprint := fs.String("fingerprint", "", "expected SSH host key fingerprint (SHA256:...)")
	yes := fs.Bool("yes", false, "trust an unknown host key on first use without asking")
	reinstall := fs.Bool("reinstall", false, "install over an existing paasd")
	preflightOnly := fs.Bool("preflight-only", false, "only run the checks")
	allowCP := fs.Bool("allow-control-plane", false, "allow installing on the control plane's own machine")
	_ = fs.Parse(flags)
	if len(pos) != 1 {
		return errors.New("usage: paas node add <user@host[:port]> [--identity KEY | --password] [--roles worker,build] [--name N] [--fingerprint SHA256:...] [--yes]")
	}
	user, host, p, err := parseSSHTarget(pos[0], *port)
	if err != nil {
		return err
	}
	if u, err := url.Parse(cfg.Server); err == nil && u.Scheme != "https" {
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return fmt.Errorf("%s is not https: the SSH credentials would travel in clear text", cfg.Server)
		}
	}
	var hk struct {
		Fingerprint  string `json:"fingerprint"`
		HostKey      string `json:"host_key"`
		Address      string `json:"address"`
		ControlPlane bool   `json:"control_plane"`
	}
	if err := call(http.MethodPost, "/cluster/ssh-host-key", map[string]any{"host": host, "port": p}, &hk); err != nil {
		return err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(hk.HostKey))
	if err != nil {
		return fmt.Errorf("host key from the control plane: %w", err)
	}
	if hk.ControlPlane && !*allowCP {
		return fmt.Errorf("%s is the control plane itself; pass --allow-control-plane to install on it anyway", host)
	}
	if err := confirmHostKey(hk.Address, key, *fingerprint, *yes); err != nil {
		return err
	}
	body := map[string]any{"host": host, "port": p, "user": user, "roles": strings.Split(*roles, ","), "name": *name,
		"fingerprint": remote.Fingerprint(key), "reinstall": *reinstall, "preflight_only": *preflightOnly, "allow_control_plane": *allowCP}
	switch {
	case *usePassword:
		body["auth"] = "password"
		body["password"] = prompt(fmt.Sprintf("SSH password for %s@%s: ", user, host), true)
	default:
		path := *identity
		if path == "" {
			ids := defaultIdentities()
			if len(ids) == 0 {
				return errors.New("no private key found in ~/.ssh: pass --identity or --password")
			}
			path = ids[0]
		}
		k, pass, err := readIdentity(path)
		if err != nil {
			return err
		}
		body["auth"] = "key"
		body["private_key"] = string(k)
		body["passphrase"] = pass
		if *sudoPassword {
			body["password"] = prompt(fmt.Sprintf("sudo password for %s: ", user), true)
		}
	}
	var inst struct {
		ID string `json:"id"`
	}
	if err := call(http.MethodPost, "/cluster/ssh-installs", body, &inst); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "install %s started; the credentials are used for this install only and not stored\n", inst.ID)
	status, err := stream("/cluster/ssh-installs/" + inst.ID + "/logs/stream")
	if err != nil {
		return err
	}
	if status != "succeeded" {
		var out struct {
			Error string `json:"error"`
		}
		_ = call(http.MethodGet, "/cluster/ssh-installs/"+inst.ID, nil, &out)
		return fmt.Errorf("install %s: %s", status, out.Error)
	}
	return nil
}

func cmdNodeInstalls() error {
	var list []struct {
		ID        string    `json:"id"`
		Host      string    `json:"host"`
		Port      int       `json:"port"`
		User      string    `json:"user"`
		Roles     []string  `json:"roles"`
		Status    string    `json:"status"`
		Step      string    `json:"step"`
		Error     string    `json:"error"`
		By        string    `json:"created_by_email"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := call(http.MethodGet, "/cluster/ssh-installs", nil, &list); err != nil {
		return err
	}
	for _, i := range list {
		fmt.Printf("%s  %s  %s@%s:%d  %s  %s/%s  by %s  %s\n", i.ID[:8], i.CreatedAt.Local().Format("2006-01-02 15:04"), i.User, i.Host, i.Port,
			strings.Join(i.Roles, ","), i.Status, i.Step, i.By, i.Error)
	}
	return nil
}

// cmdInstallSSH installs a control plane on a fresh server from this machine:
// paas install --ssh root@1.2.3.4 --domain apps.example.com --email you@example.com
func cmdInstallSSH(args []string) error {
	_, flags, passthrough := splitArgs(args, "password", "yes", "https", "ssh-agent", "reinstall", "no-zram", "no-trivy")
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	target := fs.String("ssh", "", "user@host[:port] of the server")
	port := fs.Int("port", 22, "SSH port")
	identity := fs.String("identity", "", "private key file")
	usePassword := fs.Bool("password", false, "log in with a password (asked for)")
	useAgent := fs.Bool("ssh-agent", false, "authenticate with the SSH agent (default when no --identity)")
	fingerprint := fs.String("fingerprint", "", "expected SSH host key fingerprint (SHA256:...)")
	yes := fs.Bool("yes", false, "trust an unknown host key on first use without asking")
	domain := fs.String("domain", "", "base domain for app URLs (wildcard DNS *.DOMAIN -> the server)")
	email := fs.String("email", "", "email for Let's Encrypt (enables HTTPS)")
	https := fs.Bool("https", false, "enable HTTPS without an email")
	dash := fs.String("dashboard-domain", "", "dashboard hostname (default paas.DOMAIN)")
	mirror := fs.String("mirror", "", "Docker Hub pull-through mirror")
	binary := fs.String("binary", "", "paasd binary URL (default: latest release)")
	serverName := fs.String("server-name", "", "name of this control plane")
	noZram := fs.Bool("no-zram", false, "no compressed swap in RAM")
	noTrivy := fs.Bool("no-trivy", false, "no image vulnerability scanner")
	reinstall := fs.Bool("reinstall", false, "install over an existing paasd")
	installerURL := fs.String("installer-url", defaultInstallerURL, "install.sh to run")
	_ = fs.Parse(flags)
	if *target == "" || *domain == "" {
		return errors.New("usage: paas install --ssh user@host[:port] --domain apps.example.com (--email you@example.com | --https) [--dashboard-domain D] [--identity KEY | --password | --ssh-agent] [--fingerprint SHA256:...] [-- extra install.sh flags]")
	}
	user, host, p, err := parseSSHTarget(*target, *port)
	if err != nil {
		return err
	}
	if *dash == "" {
		*dash = "paas." + *domain
	}
	ctx := context.Background()
	addr := remote.Addr(host, p)

	// 1. Host key: confirmed before any credential is sent.
	key, err := remote.ProbeHostKey(ctx, addr, remote.DefaultDialTimeout)
	if err != nil {
		return err
	}
	if err := confirmHostKey(addr, key, *fingerprint, *yes); err != nil {
		return err
	}
	dc := remote.DialConfig{Addr: addr, User: user, HostKey: remote.FingerprintCallback(remote.Fingerprint(key))}
	var password string
	switch {
	case *usePassword:
		password = prompt(fmt.Sprintf("SSH password for %s@%s: ", user, host), true)
		dc.Password = password
	case *identity != "":
		if dc.PrivateKey, dc.Passphrase, err = readIdentity(*identity); err != nil {
			return err
		}
	default:
		sock := os.Getenv("SSH_AUTH_SOCK")
		if sock != "" {
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return fmt.Errorf("ssh-agent: %w", err)
			}
			defer conn.Close()
			signers, err := agent.NewClient(conn).Signers()
			if err != nil {
				return fmt.Errorf("ssh-agent: %w", err)
			}
			dc.Signers = signers
		} else if *useAgent {
			return errors.New("--ssh-agent: SSH_AUTH_SOCK is not set")
		}
		if len(dc.Signers) == 0 {
			ids := defaultIdentities()
			if len(ids) == 0 {
				return errors.New("no SSH agent and no key in ~/.ssh: pass --identity or --password")
			}
			if dc.PrivateKey, dc.Passphrase, err = readIdentity(ids[0]); err != nil {
				return err
			}
		}
	}
	c, err := remote.Dial(ctx, dc)
	if err != nil {
		return err
	}
	defer c.Close()
	h := &remote.Host{R: c, User: user, Password: password}
	if password == "" && user != "root" {
		// Key or agent login as a non-root user: sudo may want a password.
		if _, err := remote.Output(ctx, c, "sudo -n true", nil); err != nil && term.IsTerminal(int(os.Stdin.Fd())) {
			h.Password = prompt(fmt.Sprintf("sudo password for %s: ", user), true)
		}
	}

	// 2. Preflight.
	fmt.Println("==> preflight checks")
	var rep *remote.Report
	err = remote.WithTimeout(ctx, "preflight", 2*time.Minute, func(ctx context.Context) error {
		var err error
		rep, err = remote.Preflight(ctx, h, remote.PreflightOptions{Mode: "server", Reinstall: *reinstall})
		return err
	})
	if err != nil {
		return err
	}
	for _, ch := range rep.Checks {
		mark := map[string]string{remote.CheckOK: "ok  ", remote.CheckWarn: "warn", remote.CheckFail: "FAIL"}[ch.Status]
		fmt.Printf("  [%s] %s: %s\n", mark, ch.Title, ch.Detail)
	}
	if rep.Failed() {
		return errors.New("preflight failed; fix the items marked FAIL and run again")
	}

	// 3. DNS (warn only: certificates and app URLs need it, the installation does not).
	serverIPs := append([]string{}, rep.IPs...)
	if ips, err := net.LookupHost(host); err == nil {
		serverIPs = append(serverIPs, ips...)
	}
	dnsOK := checkDNS("*."+*domain, "paas-dns-check."+*domain, serverIPs)
	dnsOK = checkDNS(*dash, *dash, serverIPs) && dnsOK

	// 4. Install.
	cmd := "curl -fsSL " + remote.Quote(*installerURL) + " | sh -s -- server --domain " + remote.Quote(*domain) + " --dashboard-domain " + remote.Quote(*dash)
	if *email != "" {
		cmd += " --email " + remote.Quote(*email)
	}
	if *https {
		cmd += " --https"
	}
	for flagName, v := range map[string]string{"--mirror": *mirror, "--binary": *binary, "--server-name": *serverName} {
		if v != "" {
			cmd += " " + flagName + " " + remote.Quote(v)
		}
	}
	if *noZram {
		cmd += " --no-zram"
	}
	if *noTrivy {
		cmd += " --no-trivy"
	}
	for _, a := range passthrough {
		cmd += " " + remote.Quote(a)
	}
	fmt.Printf("==> running %s on %s\n", *installerURL, addr)
	lw := remote.NewLineWriter(func(l string) { fmt.Println(l) }, h.Password, dc.Passphrase)
	err = remote.WithTimeout(ctx, "install", 30*time.Minute, func(ctx context.Context) error { return h.RunRoot(ctx, cmd, lw) })
	_ = lw.Close()
	if err != nil {
		return fmt.Errorf("installer: %w", err)
	}

	// 5. Wait until the dashboard answers.
	scheme := "http"
	if *email != "" || *https {
		scheme = "https"
	}
	dashURL := scheme + "://" + *dash
	fmt.Printf("==> waiting for %s/healthz\n", dashURL)
	if !waitHealthy(dashURL+"/healthz", 5*time.Minute) {
		direct := "http://" + net.JoinHostPort(host, "8080")
		if len(rep.IPs) > 0 && net.ParseIP(host) == nil {
			direct = "http://" + net.JoinHostPort(rep.IPs[0], "8080")
		}
		if waitHealthy(direct+"/healthz", 30*time.Second) {
			fmt.Printf("%s is not reachable yet (DNS or certificate), but paasd answers on %s\n", dashURL, direct)
		} else {
			fmt.Printf("%s does not answer yet; check on the server: journalctl -u paasd -f\n", dashURL)
		}
		if !dnsOK {
			fmt.Printf("Point *.%s and %s to %s, then open %s.\n", *domain, *dash, host, dashURL)
		}
	}
	fmt.Printf(`
paas is installed.
  Dashboard: %s
  Next steps:
    1. Open the dashboard and create the administrator account (the first account becomes the administrator).
    2. paas login --server %s
    3. Add nodes: Nodes > Add node > Install over SSH, or: paas node add root@<ip> --roles worker
`, dashURL, dashURL)
	return nil
}

// checkDNS warns when name does not resolve to one of the server's addresses.
func checkDNS(label, name string, serverIPs []string) bool {
	ips, err := net.LookupHost(name)
	if err != nil {
		fmt.Printf("  [warn] DNS %s does not resolve (%v); point it to the server\n", label, err)
		return false
	}
	for _, ip := range ips {
		for _, s := range serverIPs {
			if net.ParseIP(ip).Equal(net.ParseIP(s)) {
				fmt.Printf("  [ok  ] DNS %s -> %s\n", label, ip)
				return true
			}
		}
	}
	fmt.Printf("  [warn] DNS %s -> %s, not this server (%s)\n", label, strings.Join(ips, ", "), strings.Join(serverIPs, ", "))
	return false
}

// waitHealthy polls url until it answers 200 or d passes. A certificate still being issued
// fails the TLS handshake for a while, so it is simply retried.
func waitHealthy(u string, d time.Duration) bool {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		resp, err := client.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return false
}
