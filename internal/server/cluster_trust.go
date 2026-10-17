package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/pki"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

const (
	settingCACert = "cluster_ca_cert"
	settingCAKey  = "cluster_ca_key" // encrypted with the server's secrets.Box

	joinTokenTTL = time.Hour
)

// initTrust loads (or creates, once for the whole cluster) the CA and builds the NATS authenticator.
func (s *Server) initTrust(ctx context.Context) error {
	if err := s.initCA(ctx); err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	s.internalSecret = hex.EncodeToString(raw)
	s.auth = &bus.Auth{Internal: s.internalSecret, Lookup: s.lookupNkey}
	if s.cfg.NATSLegacy {
		// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
		s.auth.Legacy = s.ClusterToken
		slog.Warn("PAAS_NATS_LEGACY_TOKEN is on: the shared cluster token gives full NATS access; join every node again, then turn it off")
	}
	return nil
}

func (s *Server) lookupNkey(nkey string) (string, []string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	n, err := s.store.NodeByNkey(ctx, nkey)
	if err != nil {
		return "", nil, false
	}
	return n.ID, n.Roles, true
}

// initCA reads the CA from settings, creating it if absent. The advisory lock makes the
// check-then-create atomic across control planes, so cert and key can never come from two CAs.
func (s *Server) initCA(ctx context.Context) error {
	release := s.store.AdvisoryLock(ctx, "cluster-ca")
	defer release()
	certPEM, err := s.store.GetSetting(ctx, settingCACert)
	if errors.Is(err, store.ErrNotFound) {
		cert, key, err := pki.NewCA()
		if err != nil {
			return err
		}
		enc, err := s.box.Encrypt(string(key))
		if err != nil {
			return err
		}
		// Key first: a crash between the two writes leaves no cert, so the next start recreates both.
		if err := s.store.SetSetting(ctx, settingCAKey, enc); err != nil {
			return err
		}
		if err := s.store.SetSetting(ctx, settingCACert, string(cert)); err != nil {
			return err
		}
		certPEM = string(cert)
	} else if err != nil {
		return err
	}
	enc, err := s.store.GetSetting(ctx, settingCAKey)
	if err != nil {
		return fmt.Errorf("cluster CA key: %w", err)
	}
	keyPEM, err := s.box.Decrypt(enc)
	if err != nil {
		return fmt.Errorf("cluster CA key: %w", err)
	}
	s.caPEM = certPEM
	s.ca, err = pki.ParseCA([]byte(certPEM), []byte(keyPEM))
	return err
}

type kickMsg struct {
	Nkey string `json:"nkey"`
}

// onKick disconnects the node's connections held by this control plane.
func (s *Server) onKick(m *nats.Msg) {
	var k kickMsg
	if json.Unmarshal(m.Data, &k) == nil && k.Nkey != "" {
		s.auth.Kick(s.ns, k.Nkey)
	}
}

func hashJoinToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// handleCreateJoinToken mints a one-time join token. The command it returns carries the
// token over PublicURL, so it is refused when that URL is not https.
func (s *Server) handleCreateJoinToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		Roles []string `json:"roles"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if len(in.Roles) == 0 {
		in.Roles = []string{"worker"}
	}
	for _, role := range in.Roles {
		if role != "worker" && role != "build" && role != "edge" {
			writeError(w, 400, "role must be worker, build or edge")
			return
		}
	}
	if !strings.HasPrefix(s.cfg.PublicURL, "https://") {
		writeError(w, 409, "PAAS_PUBLIC_URL must be https:// to join nodes: the join token would travel in clear text")
		return
	}
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	tok := hex.EncodeToString(raw[:])
	exp, err := s.store.CreateJoinToken(r.Context(), hashJoinToken(tok), in.Roles, joinTokenTTL, currentUser(r).ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	cmd := fmt.Sprintf("curl -fsSL %s/install.sh | sudo sh -s -- join --url %s --token %s --roles %s",
		s.cfg.PublicURL, s.cfg.PublicURL, tok, strings.Join(in.Roles, ","))
	writeJSON(w, 200, map[string]any{"token": tok, "expires_at": exp, "command": cmd})
}

// handleEnroll trades a join token and a node public key for an identity. No login: the token is the credential.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
		Name  string `json:"name"`
		Nkey  string `json:"nkey"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || !nkeys.IsValidPublicUserKey(in.Nkey) {
		writeError(w, 400, "name and a valid nkey (public user key) are required")
		return
	}
	key := "enroll|" + clientAddr(r)
	if !loginGuard.allowed(key, maxPerEmailIP) {
		writeError(w, 429, "too many failed attempts; try again in 15 minutes")
		return
	}
	n, oldNkey, err := s.store.EnrollNode(r.Context(), hashJoinToken(in.Token), in.Name, in.Nkey, network.MaxNodes(s.clusterBase))
	if errors.Is(err, store.ErrInvalidJoinToken) {
		loginGuard.fail(key)
		writeError(w, 401, err.Error())
		return
	}
	if errors.Is(err, store.ErrNodeNameReserved) {
		writeError(w, 409, err.Error())
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if oldNkey != "" && oldNkey != in.Nkey {
		_ = bus.Publish(s.nc, proto.SubjCPKick, kickMsg{Nkey: oldNkey})
	}
	slog.Info("node enrolled", "name", n.Name, "id", n.ID, "roles", n.Roles, "rejoin", oldNkey != "")
	_, port, _ := net.SplitHostPort(s.cfg.NATSAddr)
	host := strings.TrimPrefix(strings.TrimPrefix(s.cfg.PublicURL, "https://"), "http://")
	host = strings.SplitN(strings.SplitN(host, "/", 2)[0], ":", 2)[0]
	writeJSON(w, 200, map[string]string{
		"node_id":  n.ID,
		"nats_url": fmt.Sprintf("tls://%s:%s", host, port),
		"ca_pem":   s.caPEM,
	})
}
