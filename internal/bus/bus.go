// Package bus wraps NATS: an embeddable server for the control plane and
// JSON request/reply helpers used by every component.
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// StartServer runs an embedded NATS server listening on addr, protected by token.
func StartServer(addr, token, storeDir string) (*server.Server, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, err
	}
	opts := &server.Options{
		ServerName:    "paas",
		Host:          host,
		Port:          port,
		Authorization: token,
		NoSigs:        true,
		MaxPayload:    8 << 20,
		StoreDir:      storeDir,
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

// Connect dials NATS with infinite reconnects.
func Connect(url, token, name string) (*nats.Conn, error) {
	return nats.Connect(url,
		nats.Token(token),
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
	)
}

// ConnectInProcess connects to an embedded server without a network hop.
func ConnectInProcess(ns *server.Server, token, name string) (*nats.Conn, error) {
	return nats.Connect("", nats.InProcessServer(ns), nats.Token(token), nats.Name(name), nats.MaxReconnects(-1))
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
