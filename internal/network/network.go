// Package network wires containers into per-project bridge networks (CNI),
// connects nodes with a WireGuard mesh, isolates projects with nftables and
// serves internal DNS.
//
// Address plan: the cluster owns A.(B+1).0.0 … A.(B+250).255.255. Node n gets
// A.(B+n).0.0/16; inside it project p gets the /26 at offset p*64. The last
// /26 of every node (offset 0.0.255.192) is reserved for infrastructure: the
// node's WireGuard address A.(B+n).255.254.
package network

import (
	"bytes"
	"context"
	"fmt"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template"

	gocni "github.com/containerd/go-cni"
)

const (
	// MaxProjects is the exclusive upper bound of tenant network indexes (1..MaxProjects-1).
	// Index BuildNetIndex is the BuildKit bridge (pn1022) and slot 1023 is infrastructure.
	MaxProjects   = 1022
	BuildNetIndex = 1022
	maxNodes      = 250
)

// Manager owns CNI networks on one node.
type Manager struct {
	binDir   string
	dataDir  string
	mu       sync.Mutex
	base     [2]int // first two octets of the cluster space, e.g. 10.100
	node     int    // this node's index (1..250)
	networks map[int]gocni.CNI
	shared   int // net index of the shared (system) project, reachable from every project
	DNS      *DNSServer
	Mesh     *Mesh
}

func NewManager(binDir, dataDir, subnetPrefix string) (*Manager, error) {
	base, err := ParseBase(subnetPrefix)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "cni"), 0o755); err != nil {
		return nil, err
	}
	m := &Manager{binDir: binDir, dataDir: dataDir, base: base, node: 1, networks: map[int]gocni.CNI{}}
	m.DNS = NewDNSServer()
	return m, nil
}

// ParseBase parses "10.100" into its two octets.
func ParseBase(s string) ([2]int, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return [2]int{}, fmt.Errorf("cluster base must look like 10.100, got %q", s)
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || a < 1 || a > 254 || b < 0 || b > 250 {
		return [2]int{}, fmt.Errorf("invalid cluster base %q (second octet must be <= 250)", s)
	}
	return [2]int{a, b}, nil
}

// Configure sets the address layout received from the control plane.
func (m *Manager) Configure(base [2]int, nodeIndex, sharedIndex int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base != base || m.node != nodeIndex {
		m.networks = map[int]gocni.CNI{}
	}
	m.base, m.node, m.shared = base, nodeIndex, sharedIndex
}

// BridgeName returns the Linux bridge name of a project network.
func BridgeName(idx int) string { return "pn" + strconv.Itoa(idx) }

func projectOffset(idx int) (third, fourth int) {
	off := idx * 64
	return off / 256, off % 256
}

// Subnet returns the /26 and gateway of a project network on this node.
func (m *Manager) Subnet(idx int) (subnet string, gateway string) {
	m.mu.Lock()
	base, node := m.base, m.node
	m.mu.Unlock()
	return subnetFor(base, node, idx)
}

func subnetFor(base [2]int, node, idx int) (string, string) {
	t, f := projectOffset(idx)
	return fmt.Sprintf("%d.%d.%d.%d/26", base[0], base[1]+node, t, f), fmt.Sprintf("%d.%d.%d.%d", base[0], base[1]+node, t, f+1)
}

// NodeCIDR is the /16 owned by a node.
func NodeCIDR(base [2]int, node int) string {
	return fmt.Sprintf("%d.%d.0.0/16", base[0], base[1]+node)
}

// NodeMeshIP is the WireGuard address of a node.
func NodeMeshIP(base [2]int, node int) string {
	return fmt.Sprintf("%d.%d.255.254", base[0], base[1]+node)
}

// MaxNodes is how many nodes fit in the cluster space for a base.
func MaxNodes(base [2]int) int { return min(maxNodes, 255-base[1]) }

func clusterRange(base [2]int) string {
	return fmt.Sprintf("%d.%d.0.0-%d.%d.255.255", base[0], base[1]+1, base[0], base[1]+MaxNodes(base))
}

var confTmpl = template.Must(template.New("conf").Parse(`{
  "cniVersion": "1.0.0",
  "name": "paas-{{.Index}}",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "{{.Bridge}}",
      "isGateway": true,
      "ipMasq": false,
      "hairpinMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "{{.Subnet}}", "gateway": "{{.Gateway}}"}]],
        "routes": [{"dst": "0.0.0.0/0"}],
        "dataDir": "{{.IPAMDir}}"
      }
    }
  ]
}`))

func (m *Manager) network(idx int) (gocni.CNI, error) {
	if idx <= 0 || idx >= MaxProjects {
		return nil, fmt.Errorf("project network index %d out of range (1..%d)", idx, MaxProjects-1)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.networks[idx]; ok {
		return n, nil
	}
	conf, err := m.confList(idx)
	if err != nil {
		return nil, err
	}
	n, err := gocni.New(gocni.WithPluginDir([]string{m.binDir}), gocni.WithInterfacePrefix("eth"), gocni.WithMinNetworkCount(1))
	if err != nil {
		return nil, err
	}
	if err := n.Load(gocni.WithConfListBytes(conf)); err != nil {
		return nil, fmt.Errorf("load cni config: %w", err)
	}
	m.networks[idx] = n
	return n, nil
}

// confList renders the CNI configuration of network idx on this node. m.mu must be held.
func (m *Manager) confList(idx int) ([]byte, error) {
	subnet, gw := subnetFor(m.base, m.node, idx)
	var buf bytes.Buffer
	err := confTmpl.Execute(&buf, map[string]any{
		"Index": idx, "Bridge": BridgeName(idx), "Subnet": subnet, "Gateway": gw,
		"IPAMDir": filepath.Join(m.dataDir, "cni", "ipam"),
	})
	return buf.Bytes(), err
}

// BuildConfList is the CNI configuration BuildKit uses for RUN steps: bridge
// pn<BuildNetIndex>, which the firewall treats like a project that owns nothing
// in the cluster (no other project reachable, no host ports but DNS/80/443).
func (m *Manager) BuildConfList() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.confList(BuildNetIndex)
}

// Attach connects the network namespace at nsPath to the project network and returns the container IP.
func (m *Manager) Attach(ctx context.Context, idx int, id, nsPath string) (string, error) {
	n, err := m.network(idx)
	if err != nil {
		return "", err
	}
	res, err := n.Setup(ctx, id, nsPath)
	if err != nil {
		return "", fmt.Errorf("cni setup: %w", err)
	}
	ip := ""
	if iface, ok := res.Interfaces["eth0"]; ok && len(iface.IPConfigs) > 0 {
		ip = iface.IPConfigs[0].IP.String()
	}
	if ip == "" {
		return "", fmt.Errorf("cni returned no ip")
	}
	if err := upLoopback(nsPath); err != nil {
		slog.Warn("loopback up", "ns", nsPath, "err", err)
	}
	if err := m.DNS.ListenOn(m.Gateway(idx)); err != nil {
		slog.Warn("dns listen", "ip", m.Gateway(idx), "err", err)
	}
	return ip, nil
}

// Restore re-registers a network that already has containers (after an agent restart):
// it loads the CNI config and serves DNS on its gateway.
func (m *Manager) Restore(idx int) {
	if _, err := m.network(idx); err != nil {
		slog.Warn("restore network", "index", idx, "err", err)
		return
	}
	if err := m.DNS.ListenOn(m.Gateway(idx)); err != nil {
		slog.Warn("dns listen", "ip", m.Gateway(idx), "err", err)
	}
}

// EnsureDNS (re)starts DNS listeners on the gateways of all known networks.
func (m *Manager) EnsureDNS() {
	m.mu.Lock()
	idxs := make([]int, 0, len(m.networks))
	for i := range m.networks {
		idxs = append(idxs, i)
	}
	m.mu.Unlock()
	for _, i := range idxs {
		_ = m.DNS.ListenOn(m.Gateway(i))
	}
}

// Detach releases the container's address.
func (m *Manager) Detach(ctx context.Context, idx int, id, nsPath string) error {
	n, err := m.network(idx)
	if err != nil {
		return err
	}
	return n.Remove(ctx, id, nsPath)
}

// Gateway returns the gateway (and DNS server) address of a network.
func (m *Manager) Gateway(idx int) string {
	_, gw := m.Subnet(idx)
	return gw
}

// Ruleset renders the nftables configuration. It depends only on the address
// layout and the host resolvers, never on which networks currently exist.
func (m *Manager) Ruleset() string {
	m.mu.Lock()
	base, node, shared := m.base, m.node, m.shared
	m.mu.Unlock()
	return ruleset(base, node, shared, privateResolvers(hostResolvers()))
}

// privateResolvers keeps the IPv4 resolvers (host:port or bare IP) that sit in
// private, CGNAT or link-local space. Loopback, public and IPv6 are dropped:
// only private ones are blocked by the egress drop and need an exemption.
func privateResolvers(in []string) []string {
	var out []string
	for _, r := range in {
		if h, _, err := net.SplitHostPort(r); err == nil {
			r = h
		}
		ip := net.ParseIP(r).To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		if ip.IsPrivate() || ip.IsLinkLocalUnicast() || (ip[0] == 100 && ip[1]&0xc0 == 64) {
			out = append(out, ip.String())
		}
	}
	return out
}

func ruleset(base [2]int, node, shared int, resolvers []string) string {
	mesh := MeshInterface
	var b strings.Builder
	b.WriteString("table inet paas\ndelete table inet paas\ntable inet paas {\n")
	fmt.Fprintf(&b, "  set cluster {\n    type ipv4_addr; flags interval;\n    elements = { %s }\n  }\n", clusterRange(base))
	// Which bridge owns which /26 is static: the ruleset never needs to know which networks exist.
	b.WriteString("  set same {\n    type ifname . ipv4_addr\n    elements = {")
	n := 0
	for i := 1; i < MaxProjects; i++ {
		if i == shared {
			continue
		}
		t, f := projectOffset(i)
		sep := ","
		if n%6 == 0 {
			sep = ",\n     "
		}
		if n == 0 {
			sep = "\n     "
		}
		fmt.Fprintf(&b, "%s %q . 0.0.%d.%d", sep, BridgeName(i), t, f)
		n++
	}
	b.WriteString("\n    }\n  }\n")
	b.WriteString("  chain forward {\n    type filter hook forward priority -5; policy accept;\n")
	b.WriteString("    ct state established,related accept\n")
	if len(resolvers) > 0 {
		fmt.Fprintf(&b, "    iifname \"pn*\" ip daddr { %s } meta l4proto { tcp, udp } th dport 53 accept\n", strings.Join(resolvers, ", "))
	}
	b.WriteString("    iifname \"pn*\" ip daddr { 169.254.0.0/16, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10 } ip daddr != @cluster drop\n")
	// Into a bridge: infrastructure proxies, the same project from another node, shared DB ports; the rest of the cluster is dropped.
	b.WriteString("    oifname \"pn*\" ip saddr & 0.0.255.192 == 0.0.255.192 ip saddr @cluster accept\n")
	b.WriteString("    oifname . ip saddr & 0.0.255.192 @same ip saddr @cluster accept\n")
	if shared > 0 {
		fmt.Fprintf(&b, "    oifname %q tcp dport { 3306, 5432 } ip saddr @cluster accept\n", BridgeName(shared))
	}
	b.WriteString("    oifname \"pn*\" ip saddr @cluster drop\n")
	// Out to the mesh: only towards the same project's /26 on other nodes (or the shared project).
	fmt.Fprintf(&b, "    iifname . ip daddr & 0.0.255.192 @same oifname %q accept\n", mesh)
	if shared > 0 {
		t, f := projectOffset(shared)
		fmt.Fprintf(&b, "    iifname \"pn*\" oifname %q ip daddr & 0.0.255.192 == 0.0.%d.%d accept\n", mesh, t, f)
	}
	fmt.Fprintf(&b, "    iifname \"pn*\" oifname %q drop\n", mesh)
	b.WriteString("  }\n")
	// Containers may only use the host for DNS and the HTTP(S) proxy.
	b.WriteString("  chain input {\n    type filter hook input priority 0; policy accept;\n")
	b.WriteString("    iifname != \"pn*\" accept\n")
	b.WriteString("    ct state established,related accept\n")
	b.WriteString("    meta l4proto { tcp, udp } th dport 53 accept\n")
	b.WriteString("    tcp dport { 80, 443 } accept\n")
	b.WriteString("    drop\n  }\n}\n")

	b.WriteString("table ip paas_nat\ndelete table ip paas_nat\ntable ip paas_nat {\n")
	b.WriteString("  chain postrouting {\n    type nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "    ip saddr %s ip daddr != %s oifname != \"pn*\" oifname != %q masquerade\n", NodeCIDR(base, node), clusterRange(base), mesh)
	b.WriteString("  }\n}\n")
	return b.String()
}

// SyncFirewall applies the isolation and NAT rules. The ruleset depends only on the
// address layout, so one call after registration (before any container starts)
// covers every project network, present or future.
func (m *Manager) SyncFirewall() error {
	if _, err := exec.LookPath("nft"); err != nil {
		return fmt.Errorf("nft not found: it is required for project isolation and NAT")
	}
	rules := m.Ruleset()
	// "delete table" fails when the table does not exist yet, so declare it first (done in Ruleset).
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	allowInIptablesForward()
	return nil
}

// allowInIptablesForward lets container traffic through an iptables FORWARD
// chain with a DROP policy (installed by Docker, ufw, firewalld…). Every base
// chain on the forward hook must accept a packet, so our own isolation in
// "inet paas" (which runs first) is not enough on such hosts. Docker's
// DOCKER-USER chain is used when present because Docker rebuilds FORWARD.
func allowInIptablesForward() {
	ipt, err := exec.LookPath("iptables")
	if err != nil {
		return
	}
	chain := "FORWARD"
	if exec.Command(ipt, "-w", "-L", "DOCKER-USER", "-n").Run() == nil {
		chain = "DOCKER-USER"
	}
	for _, rule := range [][]string{
		{"-i", "pn+", "-j", "ACCEPT"},
		{"-o", "pn+", "-j", "ACCEPT"},
		{"-i", MeshInterface, "-j", "ACCEPT"},
		{"-o", MeshInterface, "-j", "ACCEPT"},
	} {
		if exec.Command(ipt, append([]string{"-w", "-C", chain}, rule...)...).Run() == nil {
			continue
		}
		if out, err := exec.Command(ipt, append([]string{"-w", "-I", chain, "1"}, rule...)...).CombinedOutput(); err != nil {
			slog.Warn("iptables forward rule", "chain", chain, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}
}

// HostIP guesses the node's primary IPv4 address.
func HostIP() string {
	conn, err := net.Dial("udp", "1.1.1.1:53")
	if err == nil {
		defer conn.Close()
		if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			return a.IP.String()
		}
	}
	return "127.0.0.1"
}

// upLoopback brings up "lo" in a container's network namespace. The bridge
// plugin only configures eth0; without lo, anything talking to 127.0.0.1
// (n8n's task runners, sidecar-style processes, health scripts) hangs.
func upLoopback(nsPath string) error {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()
	lo, err := h.LinkByName("lo")
	if err != nil {
		return err
	}
	return h.LinkSetUp(lo)
}
