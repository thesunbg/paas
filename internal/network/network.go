// Package network wires containers into per-project bridge networks (CNI),
// isolates projects from each other with nftables and serves internal DNS.
package network

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"

	gocni "github.com/containerd/go-cni"
)

// Manager owns CNI networks on one node.
type Manager struct {
	binDir   string
	dataDir  string
	base     [2]int // first two octets of the subnet space, e.g. 10.200
	mu       sync.Mutex
	networks map[int]gocni.CNI
	shared   int // net index of the shared (system) network, reachable from every project
	DNS      *DNSServer
}

func NewManager(binDir, dataDir, subnetPrefix string) (*Manager, error) {
	parts := strings.Split(subnetPrefix, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("subnet prefix must look like 10.200, got %q", subnetPrefix)
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("invalid subnet prefix %q", subnetPrefix)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "cni"), 0o755); err != nil {
		return nil, err
	}
	m := &Manager{binDir: binDir, dataDir: dataDir, base: [2]int{a, b}, networks: map[int]gocni.CNI{}}
	m.DNS = NewDNSServer()
	return m, nil
}

// SetShared declares which network index all projects may reach (shared database servers).
func (m *Manager) SetShared(idx int) {
	m.mu.Lock()
	m.shared = idx
	m.mu.Unlock()
}

// BridgeName returns the Linux bridge name of a project network.
func BridgeName(idx int) string { return "pn" + strconv.Itoa(idx) }

// Subnet returns the /24 assigned to a project network index.
func (m *Manager) Subnet(idx int) (subnet string, gateway string) {
	second := m.base[1] + idx/256
	third := idx % 256
	return fmt.Sprintf("%d.%d.%d.0/24", m.base[0], second, third), fmt.Sprintf("%d.%d.%d.1", m.base[0], second, third)
}

var confTmpl = template.Must(template.New("conf").Parse(`{
  "cniVersion": "1.0.0",
  "name": "paas-{{.Index}}",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "{{.Bridge}}",
      "isGateway": true,
      "ipMasq": true,
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.networks[idx]; ok {
		return n, nil
	}
	subnet, gw := m.Subnet(idx)
	var buf bytes.Buffer
	if err := confTmpl.Execute(&buf, map[string]any{
		"Index": idx, "Bridge": BridgeName(idx), "Subnet": subnet, "Gateway": gw,
		"IPAMDir": filepath.Join(m.dataDir, "cni", "ipam"),
	}); err != nil {
		return nil, err
	}
	n, err := gocni.New(gocni.WithPluginDir([]string{m.binDir}), gocni.WithInterfacePrefix("eth"), gocni.WithMinNetworkCount(1))
	if err != nil {
		return nil, err
	}
	if err := n.Load(gocni.WithConfListBytes(buf.Bytes())); err != nil {
		return nil, fmt.Errorf("load cni config: %w", err)
	}
	m.networks[idx] = n
	return n, nil
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
	_, gw := m.Subnet(idx)
	if err := m.DNS.ListenOn(gw); err != nil {
		slog.Warn("dns listen", "ip", gw, "err", err)
	}
	if err := m.SyncFirewall(); err != nil {
		slog.Warn("nftables sync failed", "err", err)
	}
	return ip, nil
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

// SyncFirewall rewrites the nftables table that isolates project bridges:
// traffic between two different project bridges is dropped, except towards the shared network.
func (m *Manager) SyncFirewall() error {
	if _, err := exec.LookPath("nft"); err != nil {
		return nil // nftables not available; isolation disabled
	}
	m.mu.Lock()
	idxs := make([]int, 0, len(m.networks))
	for i := range m.networks {
		idxs = append(idxs, i)
	}
	shared := m.shared
	m.mu.Unlock()
	sort.Ints(idxs)

	var b strings.Builder
	b.WriteString("table inet paas\ndelete table inet paas\ntable inet paas {\n  chain forward {\n    type filter hook forward priority -5; policy accept;\n")
	b.WriteString("    ct state established,related accept\n")
	for _, i := range idxs {
		fmt.Fprintf(&b, "    iifname %q oifname %q accept\n", BridgeName(i), BridgeName(i))
	}
	if shared > 0 {
		fmt.Fprintf(&b, "    iifname \"pn*\" oifname %q accept\n", BridgeName(shared))
	}
	b.WriteString("    iifname \"pn*\" oifname \"pn*\" drop\n  }\n}\n")
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	return nil
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
