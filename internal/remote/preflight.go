package remote

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// Check results.
const (
	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Check is one line of the preflight checklist.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
}

// Report is the outcome of the preflight checks plus what they learned about the machine.
type Report struct {
	Checks     []Check  `json:"checks"`
	Hostname   string   `json:"hostname"`
	OS         string   `json:"os"`
	Arch       string   `json:"arch"`
	MemoryMB   int      `json:"memory_mb"`
	DiskFreeMB int      `json:"disk_free_mb"`
	IPs        []string `json:"ips,omitempty"`
}

// Failed reports whether any check failed.
func (r *Report) Failed() bool {
	return slices.ContainsFunc(r.Checks, func(c Check) bool { return c.Status == CheckFail })
}

// Failures lists the titles and details of the failed checks.
func (r *Report) Failures() []string {
	var out []string
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c.Title+": "+c.Detail)
		}
	}
	return out
}

func (r *Report) add(id, title, status, detail string) {
	r.Checks = append(r.Checks, Check{ID: id, Title: title, Status: status, Detail: detail})
}

// PreflightOptions says what is about to be installed.
type PreflightOptions struct {
	Mode  string   // "join" (a node) or "server" (a control plane)
	Roles []string // join: worker, build, edge
	// ControlPlaneURL (join) must answer GET /healthz from the machine.
	ControlPlaneURL string
	// Reinstall allows installing over an existing paasd.
	Reinstall bool
}

// PreflightScript prints the sections ParsePreflight reads. It needs no root.
func PreflightScript(opts PreflightOptions) string {
	var b strings.Builder
	b.WriteString(`export LC_ALL=C
echo '@@uname'; uname -sm
echo '@@os-release'; cat /etc/os-release 2>/dev/null
echo '@@free'; free -m 2>/dev/null
echo '@@df'; df -Pm /var/lib 2>/dev/null || df -Pm / 2>/dev/null
echo '@@systemd'; if [ -d /run/systemd/system ]; then echo yes; else echo no; fi
echo '@@pkg'; command -v apt-get || command -v dnf || echo none
echo '@@curl'; command -v curl || echo none
echo '@@cgroup'; stat -fc %T /sys/fs/cgroup 2>/dev/null
echo '@@installed'; [ -e /usr/local/bin/paasd ] && echo binary; [ -e /etc/systemd/system/paasd.service ] && echo unit; true
echo '@@ports'; (ss -Hltn 2>/dev/null || netstat -ltn 2>/dev/null | tail -n +3) | awk '{print $4}'
echo '@@hostname'; hostname
echo '@@ips'; hostname -I 2>/dev/null
`)
	if opts.Mode == "join" && opts.ControlPlaneURL != "" {
		u := strings.TrimRight(opts.ControlPlaneURL, "/") + "/healthz"
		b.WriteString("echo '@@reach'; curl -sS -m 10 -o /dev/null -w '%{http_code}\\n' " + Quote(u) + " 2>&1 || true\n")
	}
	b.WriteString("echo '@@end'\n")
	return b.String()
}

// splitSections cuts the script output at its "@@name" marker lines.
func splitSections(out string) map[string]string {
	sec := map[string]string{}
	cur := ""
	var b strings.Builder
	flush := func() {
		if cur != "" {
			sec[cur] = strings.TrimSpace(b.String())
		}
		b.Reset()
	}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "@@") {
			flush()
			cur = strings.TrimPrefix(l, "@@")
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	flush()
	return sec
}

// ParseOSRelease reads /etc/os-release.
func ParseOSRelease(s string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), "=")
		if !ok || k == "" || strings.HasPrefix(k, "#") {
			continue
		}
		m[k] = strings.Trim(v, `"'`)
	}
	return m
}

// ParseFreeMB reads total memory (MB) from `free -m`.
func ParseFreeMB(s string) (int, bool) {
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && (f[0] == "Mem:" || f[0] == "Mem") {
			n, err := strconv.Atoi(f[1])
			return n, err == nil
		}
	}
	return 0, false
}

// ParseDFAvailMB reads the available space (MB) of the first filesystem in `df -Pm`.
func ParseDFAvailMB(s string) (int, string, bool) {
	for _, l := range strings.Split(s, "\n") {
		f := strings.Fields(l)
		// Filesystem 1048576-blocks Used Available Capacity Mounted-on
		if len(f) < 6 || f[0] == "Filesystem" {
			continue
		}
		n, err := strconv.Atoi(f[3])
		if err != nil {
			continue
		}
		return n, f[5], true
	}
	return 0, "", false
}

// ParseListeningPorts reads local addresses ("0.0.0.0:80", "[::]:443", "*:22") into ports.
func ParseListeningPorts(s string) map[int]bool {
	ports := map[int]bool{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		i := strings.LastIndexAny(l, ":.")
		if i < 0 {
			continue
		}
		if p, err := strconv.Atoi(l[i+1:]); err == nil && p > 0 && p < 65536 {
			ports[p] = true
		}
	}
	return ports
}

// The distributions install.sh is written for (apt or dnf).
var knownDistros = []string{"debian", "ubuntu", "fedora", "rhel", "centos", "rocky", "almalinux", "ol", "amzn"}

// ParsePreflight evaluates the output of PreflightScript.
func ParsePreflight(out string, opts PreflightOptions) Report {
	sec := splitSections(out)
	var r Report
	server := opts.Mode == "server"
	r.Hostname = sec["hostname"]
	r.IPs = strings.Fields(sec["ips"])

	// OS and architecture.
	uname := strings.Fields(sec["uname"])
	switch {
	case len(uname) < 2:
		r.add("os", "Operating system", CheckFail, "could not run uname")
	case uname[0] != "Linux":
		r.add("os", "Operating system", CheckFail, uname[0]+" is not supported: Linux is required")
	default:
		osr := ParseOSRelease(sec["os-release"])
		r.OS = osr["PRETTY_NAME"]
		if r.OS == "" {
			r.OS = osr["ID"]
		}
		ids := append([]string{osr["ID"]}, strings.Fields(osr["ID_LIKE"])...)
		pkg := sec["pkg"]
		switch {
		case pkg == "none" || pkg == "":
			r.add("os", "Operating system", CheckFail, fmt.Sprintf("%s has neither apt nor dnf: use Debian, Ubuntu, Fedora or a RHEL derivative", orUnknown(r.OS)))
		case slices.ContainsFunc(ids, func(id string) bool { return slices.Contains(knownDistros, id) }):
			r.add("os", "Operating system", CheckOK, r.OS)
		default:
			r.add("os", "Operating system", CheckWarn, orUnknown(r.OS)+" is untested (it has "+pkg+"); Debian or Ubuntu are recommended")
		}
	}
	if len(uname) >= 2 {
		switch uname[1] {
		case "x86_64", "amd64":
			r.Arch = "amd64"
		case "aarch64", "arm64":
			r.Arch = "arm64"
		}
		if r.Arch == "" {
			r.add("arch", "CPU architecture", CheckFail, uname[1]+" is not supported (amd64 or arm64)")
		} else {
			r.add("arch", "CPU architecture", CheckOK, r.Arch)
		}
	}

	if sec["systemd"] == "yes" {
		r.add("systemd", "systemd", CheckOK, "running")
	} else {
		r.add("systemd", "systemd", CheckFail, "systemd is not running (paasd runs as a systemd service)")
	}
	if sec["curl"] == "none" || sec["curl"] == "" {
		r.add("curl", "curl", CheckFail, "curl is missing: install it first (apt-get install -y curl)")
	} else {
		r.add("curl", "curl", CheckOK, sec["curl"])
	}
	if fs := sec["cgroup"]; fs == "cgroup2fs" {
		r.add("cgroup", "cgroup v2", CheckOK, "unified hierarchy")
	} else {
		r.add("cgroup", "cgroup v2", CheckWarn, "cgroup v2 is recommended (found "+orUnknown(fs)+"): memory and CPU limits may be ignored")
	}

	// Memory and disk.
	minMem, warnMem, minDisk, warnDisk := 450, 900, 3<<10, 10<<10
	if server {
		minMem, warnMem, minDisk, warnDisk = 900, 1900, 5<<10, 15<<10
	}
	if mem, ok := ParseFreeMB(sec["free"]); !ok {
		r.add("memory", "Memory", CheckWarn, "could not read free -m")
	} else {
		r.MemoryMB = mem
		switch {
		case mem < minMem:
			r.add("memory", "Memory", CheckFail, fmt.Sprintf("%d MB; at least %d MB is required", mem, minMem))
		case mem < warnMem:
			r.add("memory", "Memory", CheckWarn, fmt.Sprintf("%d MB; %d MB or more is recommended", mem, warnMem))
		default:
			r.add("memory", "Memory", CheckOK, fmt.Sprintf("%d MB", mem))
		}
	}
	if avail, mnt, ok := ParseDFAvailMB(sec["df"]); !ok {
		r.add("disk", "Disk space", CheckWarn, "could not read df")
	} else {
		r.DiskFreeMB = avail
		detail := fmt.Sprintf("%.1f GB free on %s", float64(avail)/1024, mnt)
		switch {
		case avail < minDisk:
			r.add("disk", "Disk space", CheckFail, fmt.Sprintf("%s; at least %d GB is required", detail, minDisk>>10))
		case avail < warnDisk:
			r.add("disk", "Disk space", CheckWarn, fmt.Sprintf("%s; %d GB or more is recommended", detail, warnDisk>>10))
		default:
			r.add("disk", "Disk space", CheckOK, detail)
		}
	}

	// An existing installation.
	installed := strings.Fields(sec["installed"])
	switch {
	case len(installed) == 0:
		r.add("existing", "No existing paasd", CheckOK, "not installed")
	case opts.Reinstall:
		r.add("existing", "No existing paasd", CheckWarn, "paasd is already installed; it is reinstalled (its configuration is replaced)")
	default:
		r.add("existing", "No existing paasd", CheckFail, "paasd is already installed on this machine; confirm a reinstall to replace it")
	}

	// Ports the new installation listens on.
	var need []int
	if server {
		need = []int{80, 443, 8080, 4222}
	} else if slices.Contains(opts.Roles, "edge") {
		need = []int{80, 443}
	}
	if len(need) > 0 {
		listening := ParseListeningPorts(sec["ports"])
		var busy []string
		for _, p := range need {
			if listening[p] {
				busy = append(busy, strconv.Itoa(p))
			}
		}
		switch {
		case len(busy) == 0:
			r.add("ports", "Ports free", CheckOK, "ports "+joinInts(need)+" are free")
		case len(installed) > 0 && opts.Reinstall:
			r.add("ports", "Ports free", CheckWarn, "in use (probably by the existing paasd): "+strings.Join(busy, ", "))
		default:
			r.add("ports", "Ports free", CheckFail, "already in use: "+strings.Join(busy, ", ")+" (stop the web server or service holding them)")
		}
	}

	// The node must reach the control plane to enroll and download paasd.
	if opts.Mode == "join" && opts.ControlPlaneURL != "" {
		code := strings.TrimSpace(sec["reach"])
		if code == "200" {
			r.add("control-plane", "Reaches the control plane", CheckOK, opts.ControlPlaneURL+"/healthz answered 200")
		} else {
			if code == "" || code == "000" {
				code = "no answer"
			}
			r.add("control-plane", "Reaches the control plane", CheckFail, fmt.Sprintf("GET %s/healthz from the machine: %s (check DNS, firewall and the certificate)", opts.ControlPlaneURL, lastLine(code)))
		}
	}
	return r
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(ls[len(ls)-1])
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Preflight runs the checks on h: the machine itself plus whether the login user can
// become root. err is set only when the checks could not run at all.
func Preflight(ctx context.Context, h *Host, opts PreflightOptions) (*Report, error) {
	out, err := Output(ctx, h.R, "sh -c "+Quote(PreflightScript(opts)), nil)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	if !strings.Contains(out, "@@end") {
		return nil, fmt.Errorf("preflight script failed: %v: %s", err, lastLine(MaskText(out, []string{h.Password})))
	}
	r := ParsePreflight(out, opts)
	if perr := h.Privileges(ctx); perr != nil {
		if ctx.Err() != nil {
			return nil, perr
		}
		r.add("root", "Root access", CheckFail, perr.Error())
	} else if h.Root() {
		r.add("root", "Root access", CheckOK, "logged in as root")
	} else {
		r.add("root", "Root access", CheckOK, "via sudo")
	}
	return &r, nil
}

// IsSelf reports whether ip is one of this machine's own addresses (or loopback).
func IsSelf(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
