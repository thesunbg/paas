package network

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/vishvananda/netlink"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/thesunbg/paas/internal/proto"
)

// MeshInterface is the WireGuard interface connecting nodes.
const MeshInterface = "paaswg0"

// Mesh is a WireGuard device (userspace wireguard-go, works without the kernel module)
// that routes every other node's /16 through an encrypted tunnel.
type Mesh struct {
	mu      sync.Mutex
	priv    [32]byte
	pub     [32]byte
	port    int
	dev     *device.Device
	link    netlink.Link
	base    [2]int
	node    int
	routes  map[string]bool
	applied string
}

// NewMesh loads (or creates) the node's WireGuard key pair.
func NewMesh(dataDir string, port int) (*Mesh, error) {
	m := &Mesh{port: port, routes: map[string]bool{}}
	keyPath := filepath.Join(dataDir, "wg.key")
	data, err := os.ReadFile(keyPath)
	if err == nil {
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if derr != nil || len(raw) != 32 {
			return nil, fmt.Errorf("invalid key in %s", keyPath)
		}
		copy(m.priv[:], raw)
	} else {
		if _, err := rand.Read(m.priv[:]); err != nil {
			return nil, err
		}
		m.priv[0] &= 248
		m.priv[31] = (m.priv[31] & 127) | 64
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(m.priv[:])+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	pub, err := curve25519.X25519(m.priv[:], curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	copy(m.pub[:], pub)
	return m, nil
}

// PublicKey returns the base64 public key advertised to other nodes.
func (m *Mesh) PublicKey() string { return base64.StdEncoding.EncodeToString(m.pub[:]) }

// Port is the UDP port WireGuard listens on.
func (m *Mesh) Port() int { return m.port }

// Up creates the interface with this node's mesh address.
func (m *Mesh) Up(base [2]int, node int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.base, m.node = base, node
	if m.dev == nil {
		if old, err := netlink.LinkByName(MeshInterface); err == nil {
			_ = netlink.LinkDel(old) // stale interface from a previous run
		}
		tdev, err := tun.CreateTUN(MeshInterface, 1420)
		if err != nil {
			return fmt.Errorf("create %s: %w", MeshInterface, err)
		}
		logger := device.NewLogger(device.LogLevelError, "wireguard: ")
		dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
		if err := dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hex.EncodeToString(m.priv[:]), m.port)); err != nil {
			dev.Close()
			return err
		}
		if err := dev.Up(); err != nil {
			dev.Close()
			return err
		}
		m.dev = dev
	}
	link, err := netlink.LinkByName(MeshInterface)
	if err != nil {
		return err
	}
	m.link = link
	addr, _ := netlink.ParseAddr(NodeMeshIP(base, node) + "/32")
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("assign mesh address: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	slog.Info("wireguard mesh up", "interface", MeshInterface, "address", NodeMeshIP(base, node), "port", m.port, "public_key", m.PublicKey())
	return nil
}

// SetPeers configures the other nodes and routes to their subnets.
func (m *Mesh) SetPeers(peers []proto.MeshPeer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dev == nil {
		return nil
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].NodeIndex < peers[j].NodeIndex })
	var cfg strings.Builder
	cfg.WriteString("replace_peers=true\n")
	wanted := map[string]bool{}
	for _, p := range peers {
		if p.NodeIndex == m.node || p.PublicKey == "" || p.NodeIndex <= 0 {
			continue
		}
		key, err := base64.StdEncoding.DecodeString(p.PublicKey)
		if err != nil || len(key) != 32 {
			continue
		}
		cidr := NodeCIDR(m.base, p.NodeIndex)
		fmt.Fprintf(&cfg, "public_key=%s\n", hex.EncodeToString(key))
		if p.Endpoint != "" {
			if addr, err := net.ResolveUDPAddr("udp", p.Endpoint); err == nil {
				fmt.Fprintf(&cfg, "endpoint=%s\n", addr.String())
			}
		}
		fmt.Fprintf(&cfg, "persistent_keepalive_interval=25\nreplace_allowed_ips=true\nallowed_ip=%s\n", cidr)
		wanted[cidr] = true
	}
	if cfg.String() != m.applied {
		if err := m.dev.IpcSet(cfg.String()); err != nil {
			return fmt.Errorf("configure wireguard peers: %w", err)
		}
		m.applied = cfg.String()
	}
	src := net.ParseIP(NodeMeshIP(m.base, m.node))
	for cidr := range wanted {
		_, dst, _ := net.ParseCIDR(cidr)
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: m.link.Attrs().Index, Dst: dst, Src: src}); err != nil {
			slog.Warn("mesh route", "dst", cidr, "err", err)
			continue
		}
		m.routes[cidr] = true
	}
	for cidr := range m.routes {
		if !wanted[cidr] {
			_, dst, _ := net.ParseCIDR(cidr)
			_ = netlink.RouteDel(&netlink.Route{LinkIndex: m.link.Attrs().Index, Dst: dst})
			delete(m.routes, cidr)
		}
	}
	return nil
}

// Close tears the interface down.
func (m *Mesh) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dev != nil {
		m.dev.Close()
		m.dev = nil
	}
}
