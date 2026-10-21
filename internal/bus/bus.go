// Package bus wraps NATS: an embeddable server for the control plane and
// JSON request/reply helpers used by every component.
package bus

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/thesunbg/paas/internal/pki"
)

// ClusterOptions joins embedded servers of several control planes into one NATS cluster.
type ClusterOptions struct {
	Name   string   // unique server name
	Listen string   // host:port for cluster routes, empty disables clustering
	Routes []string // nats://host:port of the other control planes
	Secret string   // shared route password (the cluster token)
	// TLS secures routes. Nil keeps routes in plaintext (tests only).
	TLS *tls.Config
	// Advertise is the host:port agents reach this server's client listener on. The
	// cluster gossips it to every connected agent, which keeps it as a fallback.
	Advertise string
}

// StartServer runs an embedded NATS server listening on addr. Clients are
// authenticated by auth; TLS is enabled for clients when tlsCfg is non-nil
// (nil means plaintext, tests only).
func StartServer(addr string, auth *Auth, tlsCfg *tls.Config, storeDir string, cluster ...ClusterOptions) (*server.Server, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	opts := &server.Options{
		ServerName: "paas",
		Host:       host,
		Port:       port,
		NoSigs:     true,
		MaxPayload: 8 << 20,
		StoreDir:   storeDir,

		CustomClientAuthentication: auth,
		AlwaysEnableNonce:          true,
	}
	if tlsCfg != nil {
		opts.TLSConfig = tlsCfg
		opts.TLSTimeout = 2
		// Legacy: agents from before the cluster CA dial plain nats://; accept them while the
		// shared token is honoured. Remove with Auth.Legacy when no node has nkey IS NULL
		// (plan 261017-cluster-trust).
		opts.AllowNonTLS = auth != nil && auth.Legacy != ""
	}
	if len(cluster) > 0 && cluster[0].Listen != "" {
		c := cluster[0]
		ch, cp, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(cp)
		if err != nil {
			return nil, err
		}
		opts.ServerName = c.Name
		opts.Cluster = server.ClusterOpts{Name: "paas", Host: ch, Port: port, Username: "paas", Password: c.Secret, TLSConfig: c.TLS}
		opts.ClientAdvertise = c.Advertise
		if c.TLS != nil {
			opts.Cluster.TLSTimeout = 2
		}
		for _, r := range c.Routes {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			u, err := url.Parse(r)
			if err != nil {
				return nil, fmt.Errorf("route %q: %w", r, err)
			}
			u.User = url.UserPassword("paas", c.Secret)
			opts.Routes = append(opts.Routes, u)
		}
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, err
	}
	go ns.Start()
	if !ns.ReadyForConnections(10 * time.Second) {
		return nil, fmt.Errorf("nats server not ready on %s", addr)
	}
	return ns, nil
}

func reconnectOpts(name string) []nats.Option {
	return []nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.RetryOnFailedConnect(true),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Warn("nats disconnected", "name", name, "err", err)
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) { slog.Info("nats reconnected", "name", name, "url", c.ConnectedUrl()) }),
	}
}

// Connect dials NATS with a shared token and infinite reconnects.
// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
func Connect(url, token, name string) (*nats.Conn, error) {
	return nats.Connect(url, append(reconnectOpts(name), nats.Token(token))...)
}

// ConnectNode dials NATS as a node: nkey signature over the server nonce, TLS
// verified against ca under the fixed server name, inbox confined to the node.
func ConnectNode(url string, seed []byte, nodeID string, ca *x509.CertPool, name string) (*nats.Conn, error) {
	kp, err := nkeys.FromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("nkey seed: %w", err)
	}
	pub, err := kp.PublicKey()
	if err != nil {
		return nil, err
	}
	return nats.Connect(url, append(reconnectOpts(name),
		nats.Nkey(pub, kp.Sign),
		nats.Secure(&tls.Config{RootCAs: ca, ServerName: pki.ServerName, MinVersion: tls.VersionTLS12}),
		nats.CustomInboxPrefix("_INBOX."+nodeID),
	)...)
}

// ConnectInProcess connects to an embedded server without a network hop,
// authenticating with the internal secret.
func ConnectInProcess(ns *server.Server, secret, name string) (*nats.Conn, error) {
	return nats.Connect("", nats.InProcessServer(ns), nats.Token(secret), nats.Name(name), nats.MaxReconnects(-1))
}

// Request sends a JSON request and decodes the JSON reply.
func Request(ctx context.Context, nc *nats.Conn, subject string, req, reply any) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	msg, err := nc.RequestWithContext(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if reply == nil {
		return nil
	}
	return json.Unmarshal(msg.Data, reply)
}

// Publish sends a JSON message.
func Publish(nc *nats.Conn, subject string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return nc.Publish(subject, data)
}

// Handle subscribes with a JSON request handler executed in its own goroutine per message.
func Handle[Req any, Rep any](nc *nats.Conn, subject string, fn func(context.Context, *Req) *Rep) (*nats.Subscription, error) {
	return nc.Subscribe(subject, func(m *nats.Msg) {
		go func() {
			var req Req
			if err := json.Unmarshal(m.Data, &req); err != nil {
				slog.Warn("bad request payload", "subject", subject, "err", err)
				return
			}
			rep := fn(context.Background(), &req)
			if m.Reply == "" || rep == nil {
				return
			}
			data, err := json.Marshal(rep)
			if err != nil {
				slog.Error("marshal reply", "subject", subject, "err", err)
				return
			}
			_ = m.Respond(data)
		}()
	})
}

// HandleQueue is Handle with a queue group: one member of the group answers each request.
func HandleQueue[Req any, Rep any](nc *nats.Conn, subject, queue string, fn func(context.Context, *Req) *Rep) (*nats.Subscription, error) {
	return nc.QueueSubscribe(subject, queue, func(m *nats.Msg) {
		go func() {
			var req Req
			if err := json.Unmarshal(m.Data, &req); err != nil {
				return
			}
			rep := fn(context.Background(), &req)
			if m.Reply == "" || rep == nil {
				return
			}
			if data, err := json.Marshal(rep); err == nil {
				_ = m.Respond(data)
			}
		}()
	})
}
