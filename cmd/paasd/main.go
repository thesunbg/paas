// Command paasd runs the platform components: control plane, node agent and edge proxy.
//
//	paasd all      # everything on one machine (default)
//	paasd server   # control plane only
//	paasd agent    # node agent (joins a control plane)
//	paasd proxy    # edge proxy (joins a control plane)
//	paasd restore-state --latest   # put a backup of the platform state back
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/agent"
	"github.com/thesunbg/paas/internal/bus"
	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/proxy"
	"github.com/thesunbg/paas/internal/server"
)

func main() {
	level := slog.LevelInfo
	if os.Getenv("PAAS_DEBUG") != "" {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch mode {
	case "all":
		err = runAll(ctx)
	case "server":
		err = runServer(ctx)
	case "agent":
		err = runAgent(ctx, nil)
	case "proxy":
		err = runProxy(ctx, nil)
	case "restore-state":
		err = runRestoreState(os.Args[2:])
	case "version":
		fmt.Println("paasd", agent.Version)
		return
	case "-h", "--help", "help":
		fmt.Println("usage: paasd [all|server|agent|proxy|restore-state|version]")
		return
	default:
		err = fmt.Errorf("unknown command %q", mode)
	}
	if err != nil && ctx.Err() == nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runServer(ctx context.Context) error {
	s, err := server.New(ctx, config.LoadServer())
	if err != nil {
		return err
	}
	return s.Run(ctx)
}

// runAll starts every component in one process; agent and proxy talk to the
// embedded NATS server in-process.
func runAll(ctx context.Context) error {
	scfg := config.LoadServer()
	s, err := server.New(ctx, scfg)
	if err != nil {
		return err
	}
	errc := make(chan error, 3)
	go func() { errc <- s.Run(ctx) }()

	// The embedded agent has a node row like any other, made here without a join token: it
	// connects with the internal secret and speaks on paas.node.<id>.* from its first message.
	acfg := config.LoadAgent()
	acfg.NodeID, err = s.EnsureLocalNode(ctx, acfg.NodeName, acfg.Roles)
	if err != nil {
		return err
	}
	agentConn, err := bus.ConnectInProcess(s.NATS(), s.InternalSecret(), "agent")
	if err != nil {
		return err
	}
	go func() { errc <- runAgentWith(ctx, agentConn, acfg) }()

	proxyConn, err := bus.ConnectInProcess(s.NATS(), s.InternalSecret(), "proxy")
	if err != nil {
		return err
	}
	go func() {
		pcfg := config.LoadProxy()
		if os.Getenv("PAAS_DASHBOARD_UPSTREAM") == "" {
			addr := scfg.HTTPAddr
			if strings.HasPrefix(addr, ":") {
				addr = "127.0.0.1" + addr
			}
			pcfg.DashboardUpstream = addr
		}
		errc <- runProxyWith(ctx, proxyConn, pcfg)
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

func runAgent(ctx context.Context, nc *nats.Conn) error {
	return runAgentWith(ctx, nc, config.LoadAgent())
}

func runAgentWith(ctx context.Context, nc *nats.Conn, cfg config.Agent) error {
	a, err := agent.New(ctx, cfg, nc)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func runProxy(ctx context.Context, nc *nats.Conn) error {
	return runProxyWith(ctx, nc, config.LoadProxy())
}

func runProxyWith(ctx context.Context, nc *nats.Conn, cfg config.Proxy) error {
	if nc == nil {
		var err error
		if nc, err = connectProxy(ctx, cfg); err != nil {
			return err
		}
	}
	return proxy.New(cfg, nc).Run(ctx)
}

// connectProxy connects a standalone proxy with the identity the agent on this host enrolled with. The
// agent may not have enrolled yet, so wait for it instead of failing into a restart loop.
func connectProxy(ctx context.Context, cfg config.Proxy) (*nats.Conn, error) {
	for warned := false; ; warned = true {
		id, err := agent.LoadIdentity(cfg.IdentityDir)
		if err == nil {
			return id.Connect("proxy")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if cfg.ClusterToken != "" {
			// Legacy: remove when no node has nkey IS NULL (plan 261017-cluster-trust).
			slog.Warn("proxy uses the legacy cluster token, re-join this node")
			return bus.Connect(cfg.NATSURL, cfg.ClusterToken, "proxy")
		}
		if !warned {
			slog.Info("waiting for the node identity (the agent enrolls first)", "dir", cfg.IdentityDir)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
