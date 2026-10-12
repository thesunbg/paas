// Command paasd runs the platform components: control plane, node agent and edge proxy.
//
//	paasd all      # everything on one machine (default)
//	paasd server   # control plane only
//	paasd agent    # node agent (joins a control plane)
//	paasd proxy    # edge proxy (joins a control plane)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
	case "version":
		fmt.Println("paasd", agent.Version)
		return
	case "-h", "--help", "help":
		fmt.Println("usage: paasd [all|server|agent|proxy|version]")
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

	agentConn, err := bus.ConnectInProcess(s.NATS(), s.ClusterToken, "agent")
	if err != nil {
		return err
	}
	go func() { errc <- runAgent(ctx, agentConn, s.ClusterToken) }()

	proxyConn, err := bus.ConnectInProcess(s.NATS(), s.ClusterToken, "proxy")
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

func runAgent(ctx context.Context, nc *nats.Conn, token ...string) error {
	cfg := config.LoadAgent()
	if len(token) > 0 {
		cfg.ClusterToken = token[0]
	}
	if cfg.ClusterToken == "" {
		return fmt.Errorf("PAAS_CLUSTER_TOKEN is required")
	}
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
		if cfg.ClusterToken == "" {
			return fmt.Errorf("PAAS_CLUSTER_TOKEN is required")
		}
		nc, err = bus.Connect(cfg.NATSURL, cfg.ClusterToken, "proxy")
		if err != nil {
			return err
		}
	}
	return proxy.New(cfg, nc).Run(ctx)
}
