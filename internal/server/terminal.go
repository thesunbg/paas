package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// Interactive terminal: the dashboard opens a WebSocket here and this control
// plane relays it to the instance's node over NATS (see proto.TermOpenRequest).
// Binary frames carry keystrokes (browser to container) and output (back);
// text frames carry JSON control messages: {cols, rows} from the browser,
// {exit} or {error} to it.

const termMaxDuration = 12 * time.Hour

// GET /api/v1/services/{id}/terminal?instance=<container>&cols=&rows= (WebSocket)
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "developer")
	if svc == nil {
		return
	}
	q := r.URL.Query()
	var target *store.Instance
	for _, inst := range s.instancesOf(r.Context(), svc.ID) {
		if inst.Status == model.InstRunning && (q.Get("instance") == "" || inst.ContainerID == q.Get("instance")) {
			target = &inst
			break
		}
	}
	if target == nil {
		writeError(w, 400, "no running instance")
		return
	}
	cols, _ := strconv.Atoi(q.Get("cols"))
	rows, _ := strconv.Atoi(q.Get("rows"))
	// Accept checks that the Origin is this host, so another site cannot open a
	// terminal with the user's session cookie.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), termMaxDuration)
	defer cancel()
	fail := func(msg string) {
		b, _ := json.Marshal(map[string]string{"error": msg})
		_ = ws.Write(ctx, websocket.MessageText, b)
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	node := target.NodeID

	// Subscribe before opening, so the first prompt is not lost.
	out := make(chan *nats.Msg, 1024)
	outSub, err := s.nc.ChanSubscribe(proto.TermOutSubject(node, sid), out)
	if err != nil {
		fail(err.Error())
		return
	}
	defer outSub.Unsubscribe()
	exit := make(chan *nats.Msg, 1)
	exitSub, err := s.nc.ChanSubscribe(proto.TermExitSubject(node, sid), exit)
	if err != nil {
		fail(err.Error())
		return
	}
	defer exitSub.Unsubscribe()
	_ = outSub.SetPendingLimits(-1, 64<<20)

	var rep proto.GenericReply
	err = s.requestAgent(ctx, node, proto.CmdTermOpen, proto.TermOpenRequest{SessionID: sid, ContainerID: target.ContainerID, Cols: cols, Rows: rows}, &rep, 30*time.Second)
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	if err != nil {
		fail("could not open a terminal: " + err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "service.terminal", svc.Slug+" "+target.ContainerID)
	slog.Info("terminal", "user", u.Email, "service", svc.Slug, "container", target.ContainerID)
	ctl := func(c proto.TermCtl) {
		b, _ := json.Marshal(c)
		_ = s.nc.Publish(proto.TermCtlSubject(node, sid), b)
	}
	defer ctl(proto.TermCtl{Close: true})

	// Browser to container.
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				_ = s.nc.Publish(proto.TermInSubject(node, sid), data)
				continue
			}
			var c proto.TermCtl
			if json.Unmarshal(data, &c) == nil && c.Cols > 0 && c.Rows > 0 {
				ctl(proto.TermCtl{Cols: c.Cols, Rows: c.Rows})
			}
		}
	}()

	// Container to browser, with a keep-alive towards the agent.
	ping := time.NewTicker(proto.TermIdleTimeout / 3)
	defer ping.Stop()
	for {
		select {
		case m := <-out:
			if err := ws.Write(ctx, websocket.MessageBinary, m.Data); err != nil {
				return
			}
		case m := <-exit:
			// Drain what was sent before the exit notice.
			for len(out) > 0 {
				_ = ws.Write(ctx, websocket.MessageBinary, (<-out).Data)
			}
			_ = ws.Write(ctx, websocket.MessageText, m.Data)
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		case <-ping.C:
			ctl(proto.TermCtl{Ping: true})
		case <-readDone:
			return
		case <-ctx.Done():
			fail("the terminal was open for too long")
			return
		}
	}
}
