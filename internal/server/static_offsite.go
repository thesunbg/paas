package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// Static sites live on the node that built them; edge proxies elsewhere fetch
// from it. With off-site storage, each live build is also archived there, and
// when its node goes offline the leader unpacks it on another node and points
// the routes at that one, so the site stays up without a rebuild.

func staticRel(serviceID, deploymentID string) string {
	return path.Join("static", serviceID, deploymentID)
}

func (s *Server) staticKey(serviceID, deploymentID string) string {
	return s.storage().Key("static", serviceID, deploymentID+".tar.gz")
}

// archiveStatic uploads a static deployment's files (best effort).
func (s *Server) archiveStatic(ctx context.Context, serviceID string, d *store.Deployment) {
	if !s.storage().Enabled() || d.BuildNodeID == nil || d.StaticPath == "" {
		return
	}
	node := *d.BuildNodeID
	file := path.Join("backups", "static", serviceID, d.ID+".tar.gz")
	var rep proto.ArchiveReply
	err := s.requestAgent(ctx, node, proto.CmdArchive, proto.ArchiveRequest{Dir: staticRel(serviceID, d.ID), File: file}, &rep, 30*time.Minute)
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	if err == nil {
		err = s.uploadLarge(ctx, node, s.staticKey(serviceID, d.ID), file, rep.Size)
	}
	_ = s.requestAgent(ctx, node, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{file}}, &proto.GenericReply{}, time.Minute)
	if err != nil {
		slog.Warn("static site off-site copy", "service", serviceID, "err", err)
		return
	}
	// Only the live build is kept off-site.
	c := s.storage()
	if objs, err := c.List(c.Key("static", serviceID) + "/"); err == nil {
		for _, o := range objs {
			if path.Base(o.Key) != d.ID+".tar.gz" {
				_ = c.Delete(o.Key)
			}
		}
	}
}

// staticFailoverTick moves the live static builds of offline nodes (leader, every few minutes).
func (s *Server) staticFailoverTick(ctx context.Context) {
	if !s.storage().Enabled() {
		return
	}
	svcs, err := s.store.AllServices(ctx)
	if err != nil {
		return
	}
	moved := false
	for i := range svcs {
		svc := &svcs[i]
		if svc.CurrentDeploymentID == nil {
			continue
		}
		d, err := s.store.DeploymentByID(ctx, *svc.CurrentDeploymentID)
		if err != nil || d.StaticPath == "" || d.BuildNodeID == nil || s.nodeOnline(ctx, d.BuildNodeID) {
			continue
		}
		node, err := s.pickReplicaNode(ctx, *d.BuildNodeID, 0)
		if err != nil {
			return
		}
		file := path.Join("backups", "static", svc.ID, "restore-"+d.ID+".tar.gz")
		if err := s.transfer(ctx, node.ID, http.MethodGet, s.staticKey(svc.ID, d.ID), file); err != nil {
			slog.Warn("static site failover: no off-site copy", "service", svc.Slug, "err", err)
			continue
		}
		var rep proto.GenericReply
		err = s.requestAgent(ctx, node.ID, proto.CmdExtract, proto.ExtractRequest{File: file, Dir: staticRel(svc.ID, d.ID), Replace: true}, &rep, 30*time.Minute)
		_ = s.requestAgent(ctx, node.ID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{file}}, &proto.GenericReply{}, time.Minute)
		if err == nil && rep.Error != "" {
			err = errors.New(rep.Error)
		}
		if err != nil {
			slog.Warn("static site failover", "service", svc.Slug, "err", err)
			continue
		}
		if err := s.store.UpdateDeploymentBuild(ctx, d.ID, node.ID); err != nil {
			continue
		}
		slog.Info("static site moved off an offline node", "service", svc.Slug, "to", node.Name)
		moved = true
	}
	if moved {
		s.publishRoutes(ctx)
	}
}
