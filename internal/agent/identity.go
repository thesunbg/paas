package agent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
)

// Identity is what a node keeps on disk after enrolling: its id, where NATS is, its nkey seed
// and the cluster CA. Files live in one 0700 directory, each 0600.
type Identity struct {
	NodeID  string
	NATSURL string
	Seed    []byte
	CA      []byte
}

type nodeFile struct {
	NodeID  string `json:"node_id"`
	NATSURL string `json:"nats_url"`
}

// EnvFile is where the installer writes the node's environment; the join token is removed from it after enrolling.
const EnvFile = "/etc/paas/paasd.env"

// LoadIdentity reads the identity in dir. A missing identity is os.ErrNotExist (wrapped).
func LoadIdentity(dir string) (*Identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "node.json"))
	if err != nil {
		return nil, err
	}
	var nf nodeFile
	if err := json.Unmarshal(raw, &nf); err != nil || nf.NodeID == "" || nf.NATSURL == "" {
		return nil, fmt.Errorf("identity %s/node.json is invalid", dir)
	}
	seed, err := os.ReadFile(filepath.Join(dir, "seed.nk"))
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, err
	}
	return &Identity{NodeID: nf.NodeID, NATSURL: nf.NATSURL, Seed: bytes.TrimSpace(seed), CA: ca}, nil
}

// Connect dials NATS with the node's nkey, verifying the server against the cluster CA.
func (id *Identity) Connect(name string) (*nats.Conn, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(id.CA) {
		return nil, errors.New("identity ca.pem holds no certificate")
	}
	return bus.ConnectNode(id.NATSURL, id.Seed, id.NodeID, pool, name)
}

// Enroll generates a keypair, trades the join token for an identity at joinURL and stores it in dir.
func Enroll(ctx context.Context, joinURL, token, name, dir string) (*Identity, error) {
	return enroll(ctx, noRedirect, joinURL, token, name, dir)
}

// noRedirect stops the join token being re-sent to a redirect target; a 3xx surfaces as a failed enroll.
var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func enroll(ctx context.Context, hc *http.Client, joinURL, token, name, dir string) (*Identity, error) {
	if !strings.HasPrefix(joinURL, "https://") {
		return nil, fmt.Errorf("PAAS_JOIN_URL must be https:// (the join token would travel in clear text), got %q", joinURL)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		return nil, err
	}
	seed, err := kp.Seed()
	if err != nil {
		return nil, err
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]string{"token": token, "name": name, "nkey": pub})
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, strings.TrimRight(joinURL, "/")+"/api/v1/cluster/enroll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enroll: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("enroll rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		NodeID  string `json:"node_id"`
		NATSURL string `json:"nats_url"`
		CAPEM   string `json:"ca_pem"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.NodeID == "" || out.NATSURL == "" || out.CAPEM == "" {
		return nil, errors.New("enroll: malformed reply from control plane")
	}
	id := &Identity{NodeID: out.NodeID, NATSURL: out.NATSURL, Seed: seed, CA: []byte(out.CAPEM)}
	if err := id.save(dir); err != nil {
		return nil, fmt.Errorf("store identity: %w", err)
	}
	return id, nil
}

// save writes node.json last: LoadIdentity treats its presence as "enrolled".
func (id *Identity) save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	nf, _ := json.Marshal(nodeFile{NodeID: id.NodeID, NATSURL: id.NATSURL})
	for _, f := range []struct {
		name string
		data []byte
	}{{"seed.nk", id.Seed}, {"ca.pem", id.CA}, {"node.json", nf}} {
		if err := writeFile0600(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return nil
}

func writeFile0600(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeEnvVar drops the KEY=... line from an env file. The token is already spent, so callers only warn on error.
func removeEnvVar(path, key string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	var keep []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), key+"=") {
			continue
		}
		keep = append(keep, l)
	}
	return os.WriteFile(path, []byte(strings.Join(keep, "\n")), st.Mode().Perm())
}

// connectStandalone connects an agent that runs in its own process: enrolled identity first, then a
// first-start enroll with the join token, then the legacy cluster token.
func connectStandalone(ctx context.Context, cfg *config.Agent) (*nats.Conn, error) {
	id, err := LoadIdentity(cfg.IdentityDir)
	switch {
	case err == nil:
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	case cfg.JoinURL != "" && cfg.JoinToken != "":
		if id, err = Enroll(ctx, cfg.JoinURL, cfg.JoinToken, cfg.NodeName, cfg.IdentityDir); err != nil {
			return nil, err
		}
		slog.Info("node enrolled", "id", id.NodeID, "dir", cfg.IdentityDir)
		if err := removeEnvVar(EnvFile, "PAAS_JOIN_TOKEN"); err != nil {
			slog.Warn("could not remove PAAS_JOIN_TOKEN from the env file (the token is already used)", "file", EnvFile, "err", err)
		}
	case cfg.ClusterToken != "":
		// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
		slog.Warn("node uses the legacy cluster token, re-join it")
		return bus.Connect(cfg.NATSURL, cfg.ClusterToken, "agent-"+cfg.NodeName)
	default:
		return nil, fmt.Errorf("no node identity in %s: set PAAS_JOIN_URL and PAAS_JOIN_TOKEN to enroll", cfg.IdentityDir)
	}
	cfg.NodeID = id.NodeID
	return id.Connect("agent-" + cfg.NodeName)
}
