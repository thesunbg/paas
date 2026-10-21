package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/proto"
)

// Interactive terminals: CmdTermOpen starts the process, then keystrokes arrive on
// the session's in subject, control messages on its ctl subject, and output goes
// out in chunks on its out subject. A session ends when the process exits, on a
// close message, or when nothing (not even a ping) arrives for TermIdleTimeout.

const termMaxSessions = 32

var sessionIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

type termSessions struct {
	mu sync.Mutex
	n  int
}

var terms termSessions

// natsWriter publishes terminal output, coalescing bursts into one message.
type natsWriter struct {
	nc      *nats.Conn
	subject string
}

func (w natsWriter) Write(p []byte) (int, error) {
	// NATS copies the payload; chunks stay well under the max payload size.
	for off := 0; off < len(p); off += 64 << 10 {
		end := min(off+64<<10, len(p))
		if err := w.nc.Publish(w.subject, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

func (a *Agent) handleTermOpen(ctx context.Context, req *proto.TermOpenRequest) *proto.GenericReply {
	if !sessionIDRe.MatchString(req.SessionID) {
		return &proto.GenericReply{Error: "invalid session id"}
	}
	terms.mu.Lock()
	if terms.n >= termMaxSessions {
		terms.mu.Unlock()
		return &proto.GenericReply{Error: "too many open terminals on this node"}
	}
	terms.n++
	terms.mu.Unlock()
	release := func() { terms.mu.Lock(); terms.n--; terms.mu.Unlock() }

	out := natsWriter{nc: a.nc, subject: proto.TermOutSubject(a.nodeID, req.SessionID)}
	t, err := a.rt.OpenTerminal(ctx, req.ContainerID, req.Cmd, req.Cols, req.Rows, out)
	if err != nil {
		release()
		return &proto.GenericReply{Error: err.Error()}
	}
	activity := make(chan struct{}, 1)
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	closeReq := make(chan struct{})
	var closeOnce sync.Once
	inSub, err := a.nc.Subscribe(proto.TermInSubject(a.nodeID, req.SessionID), func(m *nats.Msg) {
		touch()
		_, _ = t.Write(m.Data)
	})
	if err != nil {
		t.Close()
		release()
		return &proto.GenericReply{Error: err.Error()}
	}
	ctlSub, err := a.nc.Subscribe(proto.TermCtlSubject(a.nodeID, req.SessionID), func(m *nats.Msg) {
		touch()
		var c proto.TermCtl
		if json.Unmarshal(m.Data, &c) != nil {
			return
		}
		if c.Close {
			closeOnce.Do(func() { close(closeReq) })
			return
		}
		if c.Cols > 0 && c.Rows > 0 {
			_ = t.Resize(context.Background(), c.Cols, c.Rows)
		}
	})
	if err != nil {
		_ = inSub.Unsubscribe()
		t.Close()
		release()
		return &proto.GenericReply{Error: err.Error()}
	}
	slog.Info("terminal opened", "container", req.ContainerID, "session", req.SessionID[:8])
	go func() {
		defer release()
		defer func() { _ = inSub.Unsubscribe(); _ = ctlSub.Unsubscribe() }()
		idle := time.NewTimer(proto.TermIdleTimeout)
		defer idle.Stop()
		reason := "exited"
	loop:
		for {
			select {
			case <-t.Done():
				break loop
			case <-closeReq:
				reason = "closed"
				t.Close()
				break loop
			case <-activity:
				if !idle.Stop() {
					<-idle.C
				}
				idle.Reset(proto.TermIdleTimeout)
			case <-idle.C:
				reason = "idle"
				t.Close()
				break loop
			}
		}
		code := t.ExitCode()
		msg, _ := json.Marshal(proto.TermCtl{Exit: &code})
		_ = a.nc.Publish(proto.TermExitSubject(a.nodeID, req.SessionID), msg)
		slog.Info("terminal ended", "container", req.ContainerID, "session", req.SessionID[:8], "reason", reason, "exit", code)
	}()
	return &proto.GenericReply{}
}
