package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/scan"
	"github.com/thesunbg/paas/internal/store"
)

// scanDeployment scans d's image on a build node and applies the project's scan
// policy: off skips, warn records and notifies, block fails the deployment before
// it goes live. nodeID is the build node holding a built image; empty means pick
// one, which pulls the image (Docker-image deployments). Without a scan (no
// scanner on the node, an agent too old to scan, no build node, scan error) warn
// deploys and block fails closed. A non-nil error means: do not roll out.
func (s *Server) scanDeployment(ctx context.Context, svc *store.Service, d *store.Deployment, nodeID string) error {
	pol, err := s.store.ProjectScanPolicy(ctx, svc.ProjectID)
	if err != nil {
		return fmt.Errorf("vulnerability scan policy: %w", err)
	}
	if pol.Mode == scan.ModeOff {
		return nil
	}
	res, why := s.runScan(ctx, d, nodeID)
	v := scan.Decide(pol, res, why)
	rec := &store.ImageScan{
		DeploymentID: d.ID, ServiceID: svc.ID, Image: d.Image, Status: store.ScanUnavailable,
		PolicyMode: pol.Mode, Threshold: pol.Threshold, Blocked: v.Block, Message: v.Log,
	}
	if res != nil {
		rec.Status, rec.Scanner, rec.ScannerVersion, rec.DBUpdatedAt, rec.OS = store.ScanScanned, res.Scanner, res.ScannerVersion, res.DBUpdatedAt, res.OS
		rec.Critical, rec.High, rec.Medium, rec.Low, rec.Unknown, rec.Findings = res.Critical, res.High, res.Medium, res.Low, res.Unknown, res.Findings
		if !res.ScannedAt.IsZero() {
			rec.ScannedAt = res.ScannedAt
		}
	}
	if err := s.store.SaveImageScan(ctx, rec); err != nil && v.Block {
		return fmt.Errorf("record vulnerability scan: %w", err)
	}
	if v.Notify {
		verb := "deployed with"
		if v.Block {
			verb = "blocked:"
		}
		s.notify(ctx, s.teamOfService(ctx, svc), "deploy.vulnerable",
			fmt.Sprintf("%s %s %d vulnerabilit%s at or above %s (%s)", svc.Slug, verb, v.Count, plural(v.Count, "y", "ies"), pol.Threshold, scan.Summary(res)),
			map[string]any{"service": svc.ID, "deployment": d.ID, "critical": res.Critical, "high": res.High, "medium": res.Medium, "low": res.Low, "blocked": v.Block})
	}
	if v.Block {
		// The image must not go live through a rollback to this deployment either.
		_ = s.store.ClearDeploymentImage(ctx, d.ID)
		d.Image, d.PushedImage = "", ""
		return errors.New(v.Log)
	}
	s.hub.Log(d.ID, "==> "+v.Log)
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// runScan asks a build node to scan d's image. It returns the result, or why there is none.
func (s *Server) runScan(ctx context.Context, d *store.Deployment, nodeID string) (*proto.ScanResult, string) {
	pull := nodeID == ""
	if pull {
		n, err := s.pickBuildNode(ctx)
		if err != nil {
			return nil, err.Error()
		}
		nodeID = n.ID
	}
	name := nodeID
	if n, err := s.store.NodeByID(ctx, nodeID); err == nil {
		name = n.Name
	}
	req := proto.ScanRequest{Image: d.Image, Pull: pull, LogSubject: proto.DeployLogSubject(d.ID)}
	var rep proto.ScanReply
	err := s.requestAgent(ctx, nodeID, proto.CmdScan, req, &rep, 25*time.Minute)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return nil, fmt.Sprintf("build node %s does not answer scan requests (agent older than this control plane, or offline)", name)
	case err != nil:
		return nil, fmt.Sprintf("scan on %s: %v", name, err)
	case rep.Missing:
		return nil, fmt.Sprintf("trivy is not installed on build node %s", name)
	case rep.Result == nil:
		return nil, fmt.Sprintf("scan on %s failed: %s", name, firstNonEmpty(rep.Error, "no result"))
	}
	return rep.Result, ""
}

func (s *Server) handleGetDeploymentScan(w http.ResponseWriter, r *http.Request) {
	d, _ := s.deploymentFor(w, r, "viewer")
	if d == nil {
		return
	}
	x, err := s.store.ImageScanForDeployment(r.Context(), d.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "this deployment has no vulnerability scan")
		return
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, x)
}

// handleGetServiceScan returns the newest scan of a service and the policy that applies to it.
func (s *Server) handleGetServiceScan(w http.ResponseWriter, r *http.Request) {
	svc, p := s.serviceFor(w, r, "viewer")
	if svc == nil {
		return
	}
	pol, err := s.store.ProjectScanPolicy(r.Context(), p.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	x, err := s.store.LatestImageScan(r.Context(), svc.ID)
	if errors.Is(err, store.ErrNotFound) {
		x, err = nil, nil
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"scan": x, "policy": pol})
}

func (s *Server) handleGetScanPolicy(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "viewer")
	if p == nil {
		return
	}
	pol, err := s.store.ProjectScanPolicy(r.Context(), p.ID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, pol)
}

// handlePutScanPolicy sets a project's scan policy. Admins only: it can turn
// the deployment gate off.
func (s *Server) handlePutScanPolicy(w http.ResponseWriter, r *http.Request) {
	p := s.projectFor(w, r, r.PathValue("id"), "admin")
	if p == nil {
		return
	}
	var in scan.Policy
	if err := decode(r, &in); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	pol, err := in.Normalize()
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if err := s.store.SetProjectScanPolicy(r.Context(), p.ID, pol); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	u := currentUser(r)
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "project.scan_policy", p.Slug+" "+pol.Mode+" "+pol.Threshold)
	writeJSON(w, 200, pol)
}
