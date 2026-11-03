package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/update"
)

// Rolling upgrades (internal/update, server/update.go): the control plane tells this
// node to install a version and restart, one node at a time, and rolls it back if it
// does not come back healthy.

// drainWait bounds how long an update waits for builds, jobs and transfers in progress.
const drainWait = 10 * time.Minute

func (a *Agent) self() *update.Self {
	return &update.Self{DataDir: a.cfg.DataDir, Version: Version, Client: &http.Client{Timeout: 15 * time.Minute}}
}

func (a *Agent) handleUpdateBinary(ctx context.Context, req *proto.UpdateBinaryRequest) *proto.UpdateBinaryReply {
	if !a.standalone {
		// The control plane in this process updates the binary itself (paas.cp.<name>.update).
		return &proto.UpdateBinaryReply{Error: "this node is a control plane: it is updated with it", Rejected: true}
	}
	for deadline := time.Now().Add(drainWait); a.busy.Load() > 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Second)
	}
	if n := a.busy.Load(); n > 0 {
		slog.Warn("updating with commands still in progress", "count", n)
	}
	restart, err := a.self().Apply(ctx, req.Version, req.URLs, req.SHA256, req.AllowUnverified, time.Duration(req.GuardSec)*time.Second)
	if err != nil {
		return &proto.UpdateBinaryReply{Error: err.Error(), Rejected: errors.Is(err, update.ErrRejected)}
	}
	if restart {
		a.restartSoon()
	}
	return &proto.UpdateBinaryReply{}
}

func (a *Agent) handleRollbackBinary(_ context.Context, _ *struct{}) *proto.UpdateBinaryReply {
	if !a.standalone {
		return &proto.UpdateBinaryReply{Error: "this node is a control plane: roll it back with it", Rejected: true}
	}
	if err := a.self().Rollback(); err != nil {
		return &proto.UpdateBinaryReply{Error: err.Error(), Rejected: errors.Is(err, update.ErrNoPrevious)}
	}
	a.restartSoon()
	return &proto.UpdateBinaryReply{}
}

// restartSoon restarts once the reply has gone out.
func (a *Agent) restartSoon() {
	time.AfterFunc(time.Second, func() {
		exe, err := update.Executable()
		if err != nil {
			slog.Error("restart after update", "err", err)
			return
		}
		a.net.DNS.Shutdown()
		update.Restart(exe)
	})
}
