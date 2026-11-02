package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/network"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

// Moving a service with volumes to another node, and restoring one from its newest volume
// backup when its node is lost. Both are a volume_moves row whose phase is the state machine;
// the whole attempt holds the service lock (deploys, reconcile, other moves wait).
//
// A move (kind "move") copies the volumes straight from node A to node B over the mesh:
//
//	1 stopping   every instance on A stops (the downtime starts here); the service status is kept in prev_status
//	2 copying    B opens a one-shot receiver on its mesh address, A streams a tar of volumes/<id> to it
//	3 verifying  B checksums what it wrote; it must equal what A sent (bytes, entries, SHA-256)
//	4 switching  B renames the copy into volumes/<id>
//	5 starting   flip: one transaction pins the service to B; it starts there (the downtime ends)
//	6 done       A's copy is queued for removal after retain_days (volume_gc)
//
// One pass, after the stop: the downtime is the time to stop, copy and checksum the volumes
// (about size / mesh bandwidth, twice read on B), plus the start on B.
//
// Until the flip (steps 1-4) A's volumes are never written to, so any failure or an abort
// leaves the service where it was: B's copy is removed and the service starts again on A. After
// the flip, a service that does not come up on B within volumeMoveStartWait is pinned back to A
// (its copy there is the data as of the stop) and started there. A restart of paasd does the
// same from the phase (resumeVolumeMoves): before the flip it undoes, after it it finishes.
//
// A restore (kind "restore") has no source to copy from: B downloads and unpacks the backup into
// the staging directory, and the rest is the same. Its RPO is the backup's time (rpo_at): what
// was written after it is lost, and the API says so before it starts.

var (
	// volumeMoveStartWait is how long a moved service gets to come up on its new node.
	volumeMoveStartWait = 5 * time.Minute
	// volumeMoveCopyTimeout bounds one copy (the transfer itself fails sooner when it stalls).
	volumeMoveCopyTimeout = 12 * time.Hour
	// volumeMovePoll is how often a running copy looks for an abort request.
	volumeMovePoll = 2 * time.Second
)

var errMoveAborted = errors.New("aborted by an administrator")

func moveStaging(id string) string   { return path.Join("moves", id) }
func volumesDir(svcID string) string { return path.Join("volumes", svcID) }

// runVolumeMove drives one attempt to its end: right after the API created it, or after a
// restart of paasd (resumeVolumeMoves).
func (s *Server) runVolumeMove(ctx context.Context, id string) {
	m, err := s.store.VolumeMoveByID(ctx, id)
	if err != nil {
		return
	}
	unlock := s.lock(m.ServiceID)
	defer unlock()
	if m, err = s.store.VolumeMoveByID(ctx, id); err != nil || store.VolumeMoveTerminal(m.Phase) {
		return
	}
	svc, err := s.store.ServiceByID(ctx, m.ServiceID)
	if err != nil {
		_ = s.store.EndVolumeMove(ctx, m.ID, store.VolumeFailed, "service: "+err.Error())
		return
	}
	switch m.Phase {
	case store.VolumeStarting:
		s.finishVolumeMove(ctx, m, svc, time.Time{})
	case store.VolumePending:
		stopped := time.Now()
		var err error
		if m.Kind == "restore" {
			err = s.prepareVolumeRestore(ctx, m, svc)
		} else {
			stopped, err = s.prepareVolumeMove(ctx, m, svc)
		}
		if err != nil {
			phase := store.VolumeFailed
			if errors.Is(err, errMoveAborted) {
				phase = store.VolumeAborted
			}
			slog.Warn("volume move", "service", svc.Slug, "kind", m.Kind, "err", err)
			s.undoVolumeMove(ctx, m, svc, phase, err.Error())
			return
		}
		s.finishVolumeMove(ctx, m, svc, stopped)
	default:
		// paasd stopped in the middle of the copy: nothing after the stop is trusted.
		s.undoVolumeMove(ctx, m, svc, store.VolumeFailed, "interrupted by a control plane restart")
	}
}

// resumeVolumeMoves finishes or undoes the attempts a previous leader left unfinished.
func (s *Server) resumeVolumeMoves(ctx context.Context) {
	ms, err := s.store.UnfinishedVolumeMoves(ctx)
	if err != nil {
		slog.Warn("resume volume moves", "err", err)
		return
	}
	for _, m := range ms {
		go s.runVolumeMove(ctx, m.ID)
	}
}

// advance moves m to the next phase unless an abort was requested or someone else moved it.
func (s *Server) advanceVolumeMove(ctx context.Context, m *store.VolumeMove, to string) error {
	fresh, err := s.store.VolumeMoveByID(ctx, m.ID)
	if err != nil {
		return err
	}
	if fresh.AbortRequested && to != store.VolumeStarting {
		return errMoveAborted
	}
	ok, err := s.store.AdvanceVolumeMove(ctx, m.ID, m.Phase, to)
	if err != nil {
		return err
	}
	if !ok {
		if fresh, err := s.store.VolumeMoveByID(ctx, m.ID); err == nil && fresh.AbortRequested {
			return errMoveAborted
		}
		return fmt.Errorf("the move left phase %s unexpectedly", m.Phase)
	}
	m.Phase = to
	return nil
}

func agentErr(err error, rep string) error {
	if err != nil {
		return err
	}
	if rep != "" {
		return errors.New(rep)
	}
	return nil
}

// prepareVolumeMove runs steps 1-4 of a move and flips. It returns when the service stopped.
func (s *Server) prepareVolumeMove(ctx context.Context, m *store.VolumeMove, svc *store.Service) (time.Time, error) {
	var stopped time.Time
	if m.FromNode == nil || m.ToNode == nil {
		return stopped, errors.New("the source or the target node is gone")
	}
	from, err := s.store.NodeByID(ctx, *m.FromNode)
	if err != nil {
		return stopped, fmt.Errorf("source node: %w", err)
	}
	to, err := s.store.NodeByID(ctx, *m.ToNode)
	if err != nil {
		return stopped, fmt.Errorf("target node: %w", err)
	}
	if from.NetIndex == nil || to.NetIndex == nil {
		return stopped, errors.New("both nodes must be on the mesh")
	}
	if err := s.advanceVolumeMove(ctx, m, store.VolumeStopping); err != nil {
		return stopped, err
	}
	// Remembered before the stop: a failure restores it.
	m.PrevStatus = svc.Status
	if err := s.store.SetVolumeMovePrevStatus(ctx, m.ID, svc.Status); err != nil {
		return stopped, err
	}
	stopped = time.Now()
	for _, in := range s.instancesOf(ctx, svc.ID) {
		s.stopInstance(ctx, &in, 30)
	}
	if err := s.store.SetServiceStatus(ctx, svc.ID, model.StatusStopped); err != nil {
		return stopped, err
	}
	s.publishAll(ctx)
	if left := s.instancesOf(ctx, svc.ID); len(left) > 0 {
		// A container that may still run would keep writing to the volumes being copied.
		return stopped, fmt.Errorf("%d instance(s) could not be stopped on %s", len(left), from.Name)
	}

	if err := s.advanceVolumeMove(ctx, m, store.VolumeCopying); err != nil {
		return stopped, err
	}
	token := util.RandomToken(32)
	var rr proto.VolumeRecvReply
	err = s.requestAgent(ctx, to.ID, proto.CmdVolumeRecv, proto.VolumeRecvRequest{
		ID: m.ID, Token: token, From: network.NodeMeshIP(s.clusterBase, *from.NetIndex), Dir: moveStaging(m.ID),
	}, &rr, time.Minute)
	if err := agentErr(err, rr.Error); err != nil {
		return stopped, fmt.Errorf("open the receiver on %s: %w", to.Name, err)
	}
	addr := net.JoinHostPort(network.NodeMeshIP(s.clusterBase, *to.NetIndex), fmt.Sprint(rr.Port))
	sent, err := s.sendWatched(ctx, m, from.ID, proto.VolumeSendRequest{ID: m.ID, Addr: addr, Token: token, Dir: volumesDir(svc.ID)})
	if err != nil {
		return stopped, err
	}

	if err := s.advanceVolumeMove(ctx, m, store.VolumeVerifying); err != nil {
		return stopped, err
	}
	var dr proto.VolumeDigestReply
	err = s.requestAgent(ctx, to.ID, proto.CmdVolumeDigest, proto.VolumeDigestRequest{Dir: moveStaging(m.ID)}, &dr, volumeMoveCopyTimeout)
	if err := agentErr(err, dr.Error); err != nil {
		return stopped, fmt.Errorf("checksum on %s: %w", to.Name, err)
	}
	if dr.Sum != sent {
		return stopped, fmt.Errorf("verification failed: %s sent %d bytes in %d entries (%.12s), %s holds %d bytes in %d entries (%.12s)",
			from.Name, sent.Bytes, sent.Files, sent.Digest, to.Name, dr.Sum.Bytes, dr.Sum.Files, dr.Sum.Digest)
	}
	if err := s.store.SetVolumeMoveSum(ctx, m.ID, sent.Bytes, sent.Files, sent.Digest); err != nil {
		return stopped, err
	}
	m.Bytes, m.Files, m.Digest = sent.Bytes, sent.Files, sent.Digest
	return stopped, s.switchVolumeMove(ctx, m, svc, to)
}

// sendWatched runs the copy on the source node, ending it early when an abort is requested.
func (s *Server) sendWatched(ctx context.Context, m *store.VolumeMove, fromID string, req proto.VolumeSendRequest) (proto.VolumeSum, error) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	aborted := make(chan struct{})
	go func() {
		t := time.NewTicker(volumeMovePoll)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
			}
			if fresh, err := s.store.VolumeMoveByID(cctx, m.ID); err == nil && fresh.AbortRequested {
				close(aborted)
				cancel()
				return
			}
		}
	}()
	var rep proto.VolumeSendReply
	err := s.requestAgent(cctx, fromID, proto.CmdVolumeSend, req, &rep, volumeMoveCopyTimeout)
	select {
	case <-aborted:
		return proto.VolumeSum{}, errMoveAborted
	default:
	}
	if err := agentErr(err, rep.Error); err != nil {
		return proto.VolumeSum{}, fmt.Errorf("copy: %w", err)
	}
	if rep.Sent != rep.Received {
		return proto.VolumeSum{}, fmt.Errorf("copy: the receiver unpacked %d bytes in %d entries, %d in %d were sent",
			rep.Received.Bytes, rep.Received.Files, rep.Sent.Bytes, rep.Sent.Files)
	}
	return rep.Sent, nil
}

// switchVolumeMove puts the verified copy in place on the target and flips the pin.
func (s *Server) switchVolumeMove(ctx context.Context, m *store.VolumeMove, svc *store.Service, to *store.Node) error {
	if err := s.advanceVolumeMove(ctx, m, store.VolumeSwitching); err != nil {
		return err
	}
	var rep proto.GenericReply
	err := s.requestAgent(ctx, to.ID, proto.CmdVolumeCommit, proto.VolumeCommitRequest{From: moveStaging(m.ID), To: volumesDir(svc.ID)}, &rep, 5*time.Minute)
	if err := agentErr(err, rep.Error); err != nil {
		return fmt.Errorf("put the copy in place on %s: %w", to.Name, err)
	}
	ok, err := s.store.FlipVolumeMove(ctx, m)
	if err != nil {
		// The transaction may have committed before the connection broke: the row says which.
		if fresh, rerr := s.store.VolumeMoveByID(ctx, m.ID); rerr == nil && fresh.Phase == store.VolumeStarting {
			m.Phase = store.VolumeStarting
			return nil
		}
		return fmt.Errorf("switch: %w", err)
	}
	if !ok {
		return errors.New("the service changed node during the move")
	}
	m.Phase = store.VolumeStarting
	return nil
}

// prepareVolumeRestore unpacks the backup (or nothing, for empty volumes) on the target and flips.
func (s *Server) prepareVolumeRestore(ctx context.Context, m *store.VolumeMove, svc *store.Service) error {
	if m.ToNode == nil {
		return errors.New("the target node is gone")
	}
	to, err := s.store.NodeByID(ctx, *m.ToNode)
	if err != nil {
		return fmt.Errorf("target node: %w", err)
	}
	if err := s.advanceVolumeMove(ctx, m, store.VolumeCopying); err != nil {
		return err
	}
	m.PrevStatus = svc.Status
	if err := s.store.SetVolumeMovePrevStatus(ctx, m.ID, svc.Status); err != nil {
		return err
	}
	staging := moveStaging(m.ID)
	if m.BackupID != nil {
		b, err := s.store.VolumeBackupByID(ctx, *m.BackupID)
		if err != nil {
			return fmt.Errorf("the backup: %w", err)
		}
		file := path.Join("backups", "volumes", svc.ID, "restore-"+m.ID+".tar.gz")
		if err := s.transfer(ctx, to.ID, http.MethodGet, b.S3Key, file); err != nil {
			return fmt.Errorf("download the volume backup: %w", err)
		}
		var rep proto.GenericReply
		err = s.requestAgent(ctx, to.ID, proto.CmdExtract, proto.ExtractRequest{File: file, Dir: staging, Replace: true}, &rep, 2*time.Hour)
		_ = s.requestAgent(ctx, to.ID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{file}}, &proto.GenericReply{}, time.Minute)
		if err := agentErr(err, rep.Error); err != nil {
			return fmt.Errorf("unpack the volume backup: %w", err)
		}
	} else {
		// Empty volumes, as asked: an empty directory takes the place of whatever is there.
		var rep proto.GenericReply
		err := s.requestAgent(ctx, to.ID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{volumesDir(svc.ID)}}, &rep, time.Minute)
		if err := agentErr(err, rep.Error); err != nil {
			return fmt.Errorf("clear the volumes on %s: %w", to.Name, err)
		}
	}
	if err := s.advanceVolumeMove(ctx, m, store.VolumeVerifying); err != nil {
		return err
	}
	if m.BackupID != nil {
		var dr proto.VolumeDigestReply
		err := s.requestAgent(ctx, to.ID, proto.CmdVolumeDigest, proto.VolumeDigestRequest{Dir: staging}, &dr, volumeMoveCopyTimeout)
		if err := agentErr(err, dr.Error); err != nil {
			return fmt.Errorf("checksum on %s: %w", to.Name, err)
		}
		m.Bytes, m.Files, m.Digest = dr.Sum.Bytes, dr.Sum.Files, dr.Sum.Digest
		if err := s.store.SetVolumeMoveSum(ctx, m.ID, m.Bytes, m.Files, m.Digest); err != nil {
			return err
		}
		return s.switchVolumeMove(ctx, m, svc, to)
	}
	if err := s.advanceVolumeMove(ctx, m, store.VolumeSwitching); err != nil {
		return err
	}
	if ok, err := s.store.FlipVolumeMove(ctx, m); err != nil || !ok {
		if err == nil {
			err = errors.New("the service changed node during the restore")
		}
		return err
	}
	m.Phase = store.VolumeStarting
	return nil
}

// undoVolumeMove ends an attempt that did not flip: the target's copy goes and the service is
// put back as it was (started again on its node when it was running).
func (s *Server) undoVolumeMove(ctx context.Context, m *store.VolumeMove, svc *store.Service, phase, msg string) {
	if fresh, err := s.store.VolumeMoveByID(ctx, m.ID); err == nil {
		if fresh.Phase == store.VolumeStarting { // the flip committed after all
			s.finishVolumeMove(ctx, fresh, svc, time.Time{})
			return
		}
		m = fresh
	}
	if m.ToNode != nil {
		var rep proto.VolumeRecvReply
		if err := s.requestAgent(ctx, *m.ToNode, proto.CmdVolumeRecv, proto.VolumeRecvRequest{ID: m.ID, Dir: moveStaging(m.ID), Cancel: true}, &rep, time.Minute); err != nil {
			slog.Warn("volume move: remove the copy on the target", "service", svc.Slug, "err", err)
		}
		if m.Phase == store.VolumeSwitching && (svc.NodeID == nil || *svc.NodeID != *m.ToNode) {
			// The copy may already be in place under volumes/<id>: it is not the service's data.
			_ = s.requestAgent(ctx, *m.ToNode, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{volumesDir(svc.ID)}}, &proto.GenericReply{}, time.Minute)
		}
	}
	// Back as it was first, then the end is recorded: a finished attempt means a settled service.
	if m.PrevStatus != "" {
		_ = s.store.SetServiceStatus(ctx, svc.ID, m.PrevStatus)
		if m.PrevStatus == model.StatusRunning && m.Kind != "restore" {
			s.reconcileLocked(ctx, svc.ID)
		}
	}
	if err := s.store.EndVolumeMove(ctx, m.ID, phase, msg); err != nil {
		slog.Warn("volume move: record the end", "service", svc.Slug, "err", err)
	}
	s.publishAll(ctx)
	what := "Moving the volumes of"
	if m.Kind == "restore" {
		what = "Restoring the volumes of"
	}
	verb := "failed"
	if phase == store.VolumeAborted {
		verb = "was aborted"
	}
	s.notify(ctx, s.teamOfService(ctx, svc), "volume.move", fmt.Sprintf("%s %s %s: %s. It stays on its node.", what, svc.Slug, verb, msg),
		map[string]any{"service": svc.ID, "move": m.ID})
}

// finishVolumeMove runs after the flip (the service is pinned to the target): it starts there,
// or, for a move, goes back to the source when it does not.
func (s *Server) finishVolumeMove(ctx context.Context, m *store.VolumeMove, svc *store.Service, stopped time.Time) {
	toName := s.nodeName(ctx, m.ToNode)
	team := s.teamOfService(ctx, svc)
	data := map[string]any{"service": svc.ID, "move": m.ID}
	rpo := ""
	if m.Kind == "restore" {
		rpo = " with empty volumes"
		if m.RPOAt != nil {
			rpo = " from the volume backup of " + m.RPOAt.UTC().Format("2006-01-02 15:04 UTC") + " (writes after it are lost)"
		}
	}
	if m.PrevStatus != model.StatusRunning {
		// It was not running: there is nothing to start, the checksum is the proof.
		if m.PrevStatus != "" {
			_ = s.store.SetServiceStatus(ctx, svc.ID, m.PrevStatus)
		}
		_ = s.store.FinishVolumeMove(ctx, m, "")
		s.publishAll(ctx)
		s.notify(ctx, team, "volume.move", fmt.Sprintf("%s now lives on %s%s.", svc.Slug, toName, rpo), data)
		return
	}
	_ = s.store.SetServiceStatus(ctx, svc.ID, model.StatusRunning)
	err := s.startOnNode(ctx, svc.ID, *m.ToNode, volumeMoveStartWait)
	if err == nil {
		if ferr := s.store.FinishVolumeMove(ctx, m, ""); ferr != nil {
			slog.Warn("volume move: record the end", "service", svc.Slug, "err", ferr)
		}
		s.publishAll(ctx)
		down := ""
		if !stopped.IsZero() {
			down = fmt.Sprintf(" (downtime %s)", time.Since(stopped).Round(time.Second))
		}
		s.notify(ctx, team, "volume.move", fmt.Sprintf("%s moved to %s%s%s.", svc.Slug, toName, rpo, down), data)
		return
	}
	slog.Warn("volume move: the service did not start on the target", "service", svc.Slug, "err", err)
	for _, in := range s.instancesOf(ctx, svc.ID) {
		s.stopInstance(ctx, &in, 10)
	}
	if m.Kind == "restore" {
		// The lost node is no way back: the restored copy stays where it is.
		_ = s.store.EndVolumeMove(ctx, m.ID, store.VolumeFailed, fmt.Sprintf("restored on %s, but it did not start: %v", toName, err))
		s.publishAll(ctx)
		s.notify(ctx, team, "volume.move", fmt.Sprintf("%s was restored on %s%s, but it did not start: %v", svc.Slug, toName, rpo, err), data)
		return
	}
	msg := fmt.Sprintf("did not start on %s (%v): back on its previous node", toName, err)
	if ok, rerr := s.store.RollbackVolumeMove(ctx, m, msg); rerr != nil || !ok {
		slog.Error("volume move: pin back to the source", "service", svc.Slug, "ok", ok, "err", rerr)
		_ = s.store.EndVolumeMove(ctx, m.ID, store.VolumeFailed, fmt.Sprintf("did not start on %s (%v), and could not be pinned back", toName, err))
	}
	s.reconcileLocked(ctx, svc.ID)
	s.publishAll(ctx)
	s.notify(ctx, team, "volume.move", fmt.Sprintf("Moving %s failed: it %s.", svc.Slug, msg), data)
}

// startOnNode starts a running service's instances (the caller holds its lock) until one runs on
// node, retrying for up to wait.
func (s *Server) startOnNode(ctx context.Context, serviceID, node string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		s.reconcileLocked(ctx, serviceID)
		for _, in := range s.instancesOf(ctx, serviceID) {
			if in.Status == model.InstRunning && in.NodeID == node {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("no instance came up in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(volumeMovePoll):
		}
	}
}

func (s *Server) nodeName(ctx context.Context, id *string) string {
	if id == nil {
		return "a removed node"
	}
	if n, err := s.store.NodeByID(ctx, *id); err == nil {
		return n.Name
	}
	return *id
}
