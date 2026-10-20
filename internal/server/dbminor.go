package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	rt "github.com/thesunbg/paas/internal/runtime"
	"github.com/thesunbg/paas/internal/store"
)

// A database server runs the image its Spec.Image names. Out of the box that is a moving tag
// (postgres:17) that every node freezes at its first pull; once an instance reports the digest of
// the manifest it runs, Spec.Image becomes <tag>@sha256:<digest> so redeploys, reschedules and new
// standbys all run the same binary. Only update-minor moves the digest.

const digestSep = "@"

// imageTag drops the digest of a reference: postgres:17@sha256:… -> postgres:17.
func imageTag(ref string) string {
	tag, _, _ := strings.Cut(ref, digestSep)
	return tag
}

func isPinned(ref string) bool { return strings.Contains(ref, digestSep+"sha256:") }

// keepPin returns the pinned form of old when fresh is the same tag, else fresh: rewriting a
// server's spec from the catalog (resize, HA) must not unpin it.
func keepPin(old, fresh string) string {
	if isPinned(old) && imageTag(old) == fresh {
		return old
	}
	return fresh
}

// specImage is the image reference of a service spec.
func specImage(svc *store.Service) string { return firstNonEmpty(svc.Spec.Image, svc.Source.Image) }

// isEngineServer reports whether a service is a database server (the shared server, a dedicated
// one or a standby), as opposed to the registry or a pooler: those carry the engine in Language.
func isEngineServer(svc *store.Service) bool {
	_, ok := databases.Engines[svc.Spec.Language]
	return ok && svc.Type == model.TypeDatabase && svc.Source.Kind == "image" && specImage(svc) != ""
}

// pinService rewrites the image of a database server to <tag>@<digest>, in the spec and in the
// current deployment (which is what a restart or reschedule actually runs). Without force it
// only pins a service that is still on a tag.
func (s *Server) pinService(ctx context.Context, serviceID, digest string, force bool) error {
	var cur *string
	var ref string
	_, err := s.store.UpdateService(ctx, serviceID, func(f *store.Service) error {
		ref = ""
		if !isEngineServer(f) || (!force && isPinned(specImage(f))) {
			return nil
		}
		ref = imageTag(specImage(f)) + digestSep + digest
		f.Spec.Image, f.Source.Image = ref, ref
		cur = f.CurrentDeploymentID
		return nil
	})
	if err != nil || ref == "" || cur == nil {
		return err
	}
	return s.store.SetDeploymentImage(ctx, *cur, rt.NormalizeRef(ref))
}

// pinFirstDigest pins the service of an instance that just reported a digest, when the instance
// belongs to the current deployment (not a draining one).
func (s *Server) pinFirstDigest(ctx context.Context, in *store.Instance, digest string) {
	svc, err := s.store.ServiceByID(ctx, in.ServiceID)
	if err != nil || !isEngineServer(svc) || isPinned(specImage(svc)) ||
		svc.CurrentDeploymentID == nil || in.DeploymentID == nil || *svc.CurrentDeploymentID != *in.DeploymentID {
		return
	}
	if err := s.pinService(ctx, svc.ID, digest, false); err != nil {
		slog.Warn("pin image digest", "service", svc.Slug, "err", err)
	}
}

// POST /api/v1/databases/{id}/update-minor {confirm_shared}
func (s *Server) handleUpdateMinor(w http.ResponseWriter, r *http.Request) {
	db, p := s.databaseFor(w, r, "admin")
	if db == nil {
		return
	}
	u := currentUser(r)
	shared := db.Mode == "shared"
	if shared && !u.IsAdmin {
		// It restarts the server of every tenant on it: not a team's call.
		writeError(w, 403, "only a platform administrator can update a shared database server")
		return
	}
	var in struct {
		ConfirmShared bool `json:"confirm_shared"`
	}
	_ = decode(r, &in)
	if shared && !in.ConfirmShared {
		writeError(w, 400, "confirm_shared is required: every database on this server restarts")
		return
	}
	if db.Options.HA {
		writeError(w, 409, "high availability databases cannot be updated yet: turn HA off first")
		return
	}
	if db.Status != "ready" || db.ServerServiceID == nil {
		writeError(w, 409, "database is not ready")
		return
	}
	srv, err := s.store.ServiceByID(r.Context(), *db.ServerServiceID)
	if err != nil || !isEngineServer(srv) {
		writeError(w, 409, "database server not found")
		return
	}
	claimed, why := s.claimUpdateMinor(r.Context(), db, srv)
	if !claimed {
		writeError(w, 409, why)
		return
	}
	s.store.Audit(r.Context(), &u.ID, &p.TeamID, "database.update_minor", db.Name)
	go func() {
		ctx := context.Background()
		if err := s.updateMinor(ctx, db.ID, u.ID); err != nil {
			slog.Error("update minor", "db", db.Name, "err", err)
			_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "minor update failed: "+err.Error())
			return
		}
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	}()
	writeJSON(w, 202, map[string]string{"status": "upgrading"})
}

// claimUpdateMinor marks the database `upgrading`. On a shared server the check of the siblings
// and the claim happen under the lock that ensureSharedServer takes, so two databases cannot
// both start.
func (s *Server) claimUpdateMinor(ctx context.Context, db *store.Database, srv *store.Service) (bool, string) {
	if db.Mode == "shared" {
		unlock := s.lock("shared-server-" + srv.Slug)
		defer unlock()
		sibs, err := s.store.DatabasesOnServer(ctx, srv.ID)
		if err != nil {
			return false, err.Error()
		}
		for _, sib := range sibs {
			if sib.Status == "upgrading" {
				return false, fmt.Sprintf("database %s on this server is being upgraded", sib.Name)
			}
		}
	}
	ok, err := s.store.ClaimDatabaseStatus(ctx, db.ID, "ready", "upgrading")
	if err != nil {
		return false, err.Error()
	}
	if !ok {
		return false, "database is not ready"
	}
	return true, ""
}

// updateMinor moves a server to the newest build of the tag it runs (17 -> 17.x). It never
// changes the tag, so never the major. There is no way back: the pre-update backup is the restore
// point (pinned, so retention keeps it).
func (s *Server) updateMinor(ctx context.Context, dbID, userID string) error {
	unlock := s.lock("db-" + dbID)
	defer unlock()
	db, err := s.store.DatabaseByID(ctx, dbID)
	if err != nil {
		return err
	}
	if db.Options.HA { // switched on between the request and the lock
		return errors.New("high availability was turned on: turn it off first")
	}
	if db.ServerServiceID == nil {
		return errors.New("database server not found")
	}
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		return err
	}
	b, err := s.backupDatabase(ctx, db)
	if err != nil {
		return fmt.Errorf("backup before update: %w", err)
	}
	if err := s.store.PinBackup(ctx, b.ID); err != nil {
		return err
	}
	in, err := s.firstInstance(ctx, srv.ID)
	if err != nil {
		return err
	}
	// Pull first: the rollout stops the old instance before starting the new one, so a pull that
	// fails after that would leave the database down.
	ref := rt.NormalizeRef(imageTag(specImage(srv)))
	var pulled proto.RefreshImageReply
	err = s.requestAgent(ctx, in.NodeID, proto.CmdRefreshImage, proto.RefreshImageRequest{Image: ref}, &pulled, 11*time.Minute)
	if errors.Is(err, nats.ErrNoResponders) {
		return errors.New("the node's agent is too old to refresh images: update the node first")
	}
	if err == nil && pulled.Error != "" {
		err = errors.New(pulled.Error)
	}
	if err == nil && pulled.Digest == "" {
		err = errors.New("the agent did not report an image digest")
	}
	if err != nil {
		return fmt.Errorf("pull new image (database untouched): %w", err)
	}
	// Roll out the tag the node just refreshed (no network needed), pin the digest once it runs.
	d, err := s.store.CreateDeployment(ctx, &store.Deployment{
		ServiceID: srv.ID, Status: model.DeployQueued, Trigger: "update-minor", Image: ref, CreatedBy: &userID,
	})
	if err != nil {
		return err
	}
	_, _ = s.store.Q(ctx).Exec(ctx, `UPDATE deployments SET detected='image' WHERE id=$1`, d.ID)
	s.hub.Log(d.ID, "==> minor update of "+ref+" ("+pulled.Digest+")")
	kickDeploys()
	if err := s.waitDeployment(ctx, d.ID, 30*time.Minute); err != nil {
		return fmt.Errorf("rollout (the old instance was stopped; the backup taken first is pinned): %w", err)
	}
	if err := s.pinService(ctx, srv.ID, pulled.Digest, true); err != nil {
		return err
	}
	versionCache.Delete(db.ID)
	if sibs, err := s.store.DatabasesOnServer(ctx, srv.ID); err == nil {
		for _, sib := range sibs {
			versionCache.Delete(sib.ID)
		}
	}
	return nil
}

// waitDeployment blocks until a deployment is live, failing if it fails or is cancelled.
func (s *Server) waitDeployment(ctx context.Context, id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		d, err := s.store.DeploymentByID(ctx, id)
		if err != nil {
			return err
		}
		switch d.Status {
		case model.DeployActive:
			return nil
		case model.DeployFailed, model.DeployCancelled, model.DeploySuperseded:
			return fmt.Errorf("deployment %s: %s", d.Status, d.Error)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("timed out waiting for the deployment")
}
