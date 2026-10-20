package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/s3"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/util"
)

var identRe = regexp.MustCompile(`[^a-z0-9_]+`)

func dbIdent(parts ...string) string {
	s := identRe.ReplaceAllString(strings.ToLower(strings.Join(parts, "_")), "_")
	s = strings.Trim(s, "_")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		s = "db_" + s
	}
	return s
}

// CreateDatabaseInput is the API payload for a new managed database.
type CreateDatabaseInput struct {
	Name        string   `json:"name"`
	Engine      string   `json:"engine"`
	Mode        string   `json:"mode"` // shared | dedicated
	Size        string   `json:"size"`
	Version     string   `json:"version"` // engine version; empty = default
	Link        []string `json:"link_services"`
	EnvName     string   `json:"env_name"`
	BackupHours *int     `json:"backup_schedule_hours"`
}

func (s *Server) createDatabase(ctx context.Context, proj *store.Project, in CreateDatabaseInput) (*store.Database, error) {
	eng, ok := databases.Engines[in.Engine]
	if !ok {
		return nil, fmt.Errorf("unsupported engine %q (postgres, mysql, mariadb, redis, mongo)", in.Engine)
	}
	if in.Mode == "" {
		in.Mode = "shared"
		if !eng.SharedOK {
			in.Mode = "dedicated"
		}
	}
	if in.Mode == "shared" && !eng.SharedOK {
		return nil, fmt.Errorf("%s only supports dedicated mode", in.Engine)
	}
	if in.Mode != "shared" && in.Mode != "dedicated" {
		return nil, errors.New("mode must be shared or dedicated")
	}
	if in.Version == "" {
		in.Version = eng.DefaultVersion
	}
	if _, err := databases.CheckCreate(in.Engine, in.Version, time.Now()); err != nil {
		return nil, err
	}
	if in.Size == "" {
		in.Size = "micro"
	}
	if _, ok := databases.PresetByName(in.Size); !ok {
		return nil, fmt.Errorf("unknown size %q", in.Size)
	}
	name := util.Slugify(in.Name)
	if in.Name == "" {
		name = in.Engine
	}
	backupHours := 24
	if in.BackupHours != nil {
		backupHours = *in.BackupHours
	}
	// Shared servers host many tenants: a random suffix keeps identifiers unique.
	ident := dbIdent(proj.Slug, name)
	if len(ident) > 36 {
		ident = ident[:36]
	}
	ident += "_" + util.RandomToken(3)
	if in.Mode == "dedicated" {
		ident = dbIdent(name)
	}
	db, err := s.store.CreateDatabase(ctx, &store.Database{
		ProjectID: proj.ID, Name: name, Engine: in.Engine, Version: in.Version, Mode: in.Mode, DBName: ident, Username: ident,
		PasswordEnc: s.box.MustEncrypt(util.RandomPassword(24)), SizePreset: in.Size, BackupScheduleHours: backupHours,
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, fmt.Errorf("a database named %q already exists in this project", name)
		}
		return nil, err
	}
	envName := in.EnvName
	if envName == "" {
		envName = eng.DefaultEnv
	}
	for _, sid := range in.Link {
		svc, err := s.store.ServiceByID(ctx, sid)
		if err == nil && svc.ProjectID == proj.ID {
			_ = s.store.LinkDatabase(ctx, sid, db.ID, envName)
		}
	}
	go s.provisionDatabase(context.Background(), db.ID)
	return db, nil
}

func (s *Server) dbCreds(ctx context.Context, db *store.Database) (databases.Credentials, *store.Service, error) {
	pass, err := s.box.Decrypt(db.PasswordEnc)
	if err != nil {
		return databases.Credentials{}, nil, err
	}
	c := databases.Credentials{DBName: db.DBName, Username: db.Username, Password: pass, Port: databases.Engines[db.Engine].Port}
	if db.ServerServiceID == nil {
		return c, nil, errors.New("database server not provisioned yet")
	}
	srv, err := s.store.ServiceByID(ctx, *db.ServerServiceID)
	if err != nil {
		return c, nil, err
	}
	proj, err := s.store.ProjectByID(ctx, srv.ProjectID)
	if err != nil {
		return c, nil, err
	}
	c.Host = internalHost(srv.Slug, proj.Slug)
	if len(srv.Spec.DNSAliases) > 0 {
		c.Host = srv.Spec.DNSAliases[0] // HA: follows whichever server is primary
	}
	if db.Mode == "shared" {
		root, err := s.sharedRootPassword(ctx, db.Engine)
		if err != nil {
			return c, nil, err
		}
		c.RootPass = root
	}
	return c, srv, nil
}

func (s *Server) databaseURL(ctx context.Context, db *store.Database) (string, error) {
	c, _, err := s.dbCreds(ctx, db)
	if err != nil {
		return "", err
	}
	return databases.URL(db.Engine, c), nil
}

func (s *Server) sharedRootPassword(ctx context.Context, engine string) (string, error) {
	key := "shared_" + engine + "_root"
	enc, err := s.store.GetSetting(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		enc = s.box.MustEncrypt(util.RandomPassword(32))
		if err := s.store.SetSetting(ctx, key, enc); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	return s.box.Decrypt(enc)
}

func (s *Server) provisionDatabase(ctx context.Context, dbID string) {
	db, err := s.store.DatabaseByID(ctx, dbID)
	if err != nil {
		return
	}
	fail := func(err error) {
		slog.Error("provision database", "db", db.Name, "err", err)
		_ = s.store.SetDatabaseStatus(ctx, db.ID, "failed", err.Error())
	}
	var srv *store.Service
	if db.Mode == "shared" {
		srv, err = s.ensureSharedServer(ctx, db.Engine, db.Version)
		if err != nil {
			fail(err)
			return
		}
		if err := s.store.SetDatabaseServer(ctx, db.ID, srv.ID); err != nil {
			fail(err)
			return
		}
		db.ServerServiceID = &srv.ID
		c, _, err := s.dbCreds(ctx, db)
		if err != nil {
			fail(err)
			return
		}
		preset, _ := databases.PresetByName(db.SizePreset)
		cmd, err := databases.CreateLogical(db.Engine, c, preset.MaxConns/2+5)
		if err != nil {
			fail(err)
			return
		}
		if err := s.execOnService(ctx, srv.ID, cmd, "", "", 5); err != nil {
			fail(fmt.Errorf("create database on shared server: %w", err))
			return
		}
	} else {
		srv, err = s.createDedicatedServer(ctx, db, dedicatedOpts{onCreated: func(id string) error { return s.store.SetDatabaseServer(ctx, db.ID, id) }})
		if err != nil {
			fail(err)
			return
		}
	}
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "ready", "")
	slog.Info("database ready", "name", db.Name, "engine", db.Engine, "mode", db.Mode)
	s.restartLinked(ctx, db.ID)
}

func sharedSlug(engine, version string) string {
	slug := "shared-" + engine
	if version != databases.Engines[engine].DefaultVersion {
		slug += "-" + strings.ReplaceAll(version, ".", "-")
	}
	return slug
}

// ensureSharedServer returns the shared server for an engine, creating and deploying it on first use.
// Each engine version has its own shared server; the default one keeps "shared-<engine>".
func (s *Server) ensureSharedServer(ctx context.Context, engine, version string) (*store.Service, error) {
	slug := sharedSlug(engine, version)
	unlock := s.lock("shared-server-" + slug)
	defer unlock()
	services, err := s.store.ServicesForProject(ctx, s.systemProjectID)
	if err != nil {
		return nil, err
	}
	for i := range services {
		if services[i].Slug == slug {
			srv := &services[i]
			if srv.Status == model.StatusFailed || srv.Status == model.StatusStopped || srv.CurrentDeploymentID == nil && srv.Status != model.StatusDeploying {
				// Previous attempt failed: try again.
				if srv.Status == model.StatusStopped {
					_ = s.startService(ctx, srv)
				} else if _, err := s.QueueDeployment(ctx, srv, "system", "", "", nil); err != nil {
					return nil, err
				}
				_ = s.store.SetServiceStatus(ctx, srv.ID, model.StatusDeploying)
			}
			return srv, s.waitServiceRunning(ctx, srv.ID, 10*time.Minute)
		}
	}
	root, err := s.sharedRootPassword(ctx, engine)
	if err != nil {
		return nil, err
	}
	preset, _ := databases.PresetByName("small")
	spec, err := databases.ServerSpec(engine, version, preset, true, databases.Credentials{RootPass: root})
	if err != nil {
		return nil, err
	}
	srv, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: s.systemProjectID, Name: "Shared " + engine + " " + version, Slug: slug, Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: util.RandomToken(16),
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.QueueDeployment(ctx, srv, "system", "", "", nil); err != nil {
		return nil, err
	}
	return srv, s.waitServiceRunning(ctx, srv.ID, 10*time.Minute)
}

// dedicatedOpts tune createDedicatedServer. The server is never attached to the database here: the
// caller does it (provisioning at once, a move only at the flip).
type dedicatedOpts struct {
	pinNode   *string
	slug      string                // "" = the database name
	onCreated func(id string) error // the service row exists, nothing is deployed yet
}

func (s *Server) createDedicatedServer(ctx context.Context, db *store.Database, o dedicatedOpts) (*store.Service, error) {
	c, _, _ := s.dbCreds(ctx, db) // server not yet linked; host is filled later
	spec, err := s.dedicatedSpec(db, c)
	if err != nil {
		return nil, err
	}
	slug := util.Slugify(firstNonEmpty(o.slug, db.Name))
	srv, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: db.ProjectID, Name: db.Name + " (" + db.Engine + ")", Slug: slug, Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: util.RandomToken(16), NodeID: o.pinNode,
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			return nil, fmt.Errorf("a service named %q already exists in this project", slug)
		}
		return nil, err
	}
	if o.onCreated != nil {
		if err := o.onCreated(srv.ID); err != nil {
			_ = s.store.DeleteService(ctx, srv.ID)
			return nil, err
		}
	}
	if _, err := s.QueueDeployment(ctx, srv, "system", "", "", nil); err != nil {
		return srv, err
	}
	return srv, s.waitServiceRunning(ctx, srv.ID, 10*time.Minute)
}

func (s *Server) waitServiceRunning(ctx context.Context, serviceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc, err := s.store.ServiceByID(ctx, serviceID)
		if err != nil {
			return err
		}
		if svc.Status == model.StatusRunning && svc.CurrentDeploymentID != nil {
			if _, err := s.firstInstance(ctx, serviceID); err == nil {
				return nil
			}
		}
		if svc.Status == model.StatusFailed {
			deps, _ := s.store.DeploymentsForService(ctx, serviceID, 1)
			if len(deps) > 0 {
				return fmt.Errorf("database server failed to start: %s", deps[0].Error)
			}
			return errors.New("database server failed to start")
		}
		time.Sleep(time.Second)
	}
	return errors.New("timed out waiting for the database server")
}

// execOnService runs a command in the first running instance of a service, retrying while it starts up.
func (s *Server) execOnService(ctx context.Context, serviceID string, cmd databases.Command, stdinFile, stdoutFile string, attempts int) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i*2) * time.Second)
		}
		in, err := s.firstInstance(ctx, serviceID)
		if err != nil {
			lastErr = err
			continue
		}
		var rep proto.ExecReply
		err = s.requestAgent(ctx, in.NodeID, proto.CmdExec, proto.ExecRequest{
			ContainerID: in.ContainerID, Cmd: cmd.Cmd, Env: cmd.Env, Stdin: cmd.Stdin, StdinFile: stdinFile, StdoutFile: stdoutFile, TimeoutSec: 3600,
		}, &rep, time.Hour)
		if err != nil {
			lastErr = err
			continue
		}
		if rep.Error != "" {
			lastErr = errors.New(rep.Error)
			continue
		}
		if rep.ExitCode != 0 {
			lastErr = fmt.Errorf("exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
			continue
		}
		return nil
	}
	return lastErr
}

// restartLinked redeploys running services that use a database so they pick up its URL.
func (s *Server) restartLinked(ctx context.Context, dbID string) {
	links, err := s.store.LinksForDatabase(ctx, dbID)
	if err != nil {
		return
	}
	for _, l := range links {
		svc, err := s.store.ServiceByID(ctx, l.ServiceID)
		if err != nil || svc.CurrentDeploymentID == nil || svc.Status != model.StatusRunning {
			continue
		}
		if _, err := s.restartService(ctx, svc, nil); err != nil {
			slog.Warn("restart linked service", "service", svc.Slug, "err", err)
		}
	}
}

func (s *Server) deleteDatabase(ctx context.Context, db *store.Database) error {
	_ = s.store.SetDatabaseStatus(ctx, db.ID, "deleting", "")
	s.removePooler(ctx, db)
	s.removeStandby(ctx, db)
	if db.ServerServiceID != nil {
		if err := s.dropPlacement(ctx, db, *db.ServerServiceID, db.Mode == "shared"); err != nil {
			return err
		}
	}
	backups, _ := s.store.BackupsForDatabase(ctx, db.ID)
	for _, b := range backups {
		if b.NodeID != nil {
			_ = s.requestAgent(ctx, *b.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{b.Path}}, &proto.GenericReply{}, time.Minute)
		}
	}
	return s.store.DeleteDatabase(ctx, db.ID)
}

// dropPlacement removes a database from a server it lives on (or lived on): on a shared server the
// logical database and its roles (extra users first: roles are global there), on a dedicated one the
// whole server with its volume.
func (s *Server) dropPlacement(ctx context.Context, db *store.Database, serviceID string, shared bool) error {
	srv, err := s.store.ServiceByID(ctx, serviceID)
	if err != nil {
		return nil // already gone
	}
	if shared {
		at := *db
		at.ServerServiceID, at.Mode = &serviceID, "shared"
		c, _, err := s.dbCreds(ctx, &at)
		if err != nil {
			return nil
		}
		users, _ := s.store.DBUsers(ctx, db.ID)
		for _, u := range users {
			if cmd, err := databases.DropUser(db.Engine, true, c, u.Username); err == nil {
				_ = s.execOnService(ctx, srv.ID, cmd, "", "", 1)
			}
		}
		if cmd, err := databases.DropLogical(db.Engine, c); err == nil {
			if err := s.execOnService(ctx, srv.ID, cmd, "", "", 2); err != nil {
				slog.Warn("drop logical database", "db", db.Name, "err", err)
			}
		}
		return nil
	}
	node := srv.NodeID
	if err := s.deleteService(ctx, srv); err != nil {
		return err
	}
	if node != nil {
		_ = s.requestAgent(ctx, *node, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{filepath.Join("volumes", srv.ID)}}, &proto.GenericReply{}, time.Minute)
	}
	return nil
}

// backupDatabase dumps a database to a file on the node running its server.
func (s *Server) backupDatabase(ctx context.Context, db *store.Database) (*store.Backup, error) {
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return nil, err
	}
	cmd, ext, err := databases.Backup(db.Engine, db.Mode == "shared", c)
	if err != nil {
		return nil, err
	}
	in, err := s.firstInstance(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	path := filepath.Join("backups", db.ID, time.Now().UTC().Format("20060102-150405")+"."+ext)
	b, err := s.store.CreateBackup(ctx, db.ID, in.NodeID, path)
	if err != nil {
		return nil, err
	}
	var rep proto.ExecReply
	err = s.requestAgent(ctx, in.NodeID, proto.CmdExec, proto.ExecRequest{ContainerID: in.ContainerID, Cmd: cmd.Cmd, Env: cmd.Env, StdoutFile: path, TimeoutSec: 3600}, &rep, time.Hour)
	if err == nil && rep.Error != "" {
		err = errors.New(rep.Error)
	}
	if err == nil && rep.ExitCode != 0 {
		err = fmt.Errorf("exit code %d: %s", rep.ExitCode, strings.TrimSpace(rep.Stderr))
	}
	if err != nil {
		_ = s.store.FinishBackup(ctx, b.ID, "failed", err.Error(), 0)
		b.Status, b.Error = "failed", err.Error()
		return b, err
	}
	_ = s.store.FinishBackup(ctx, b.ID, "completed", "", rep.StdoutBytes)
	_ = s.store.TouchDatabaseBackup(ctx, db.ID)
	b.Status, b.SizeBytes = "completed", rep.StdoutBytes
	if s.s3.Enabled() {
		key := s.s3.Key(db.ID, filepath.Base(path))
		if err := s.transfer(ctx, in.NodeID, http.MethodPut, key, path); err != nil {
			slog.Warn("off-site backup upload failed", "db", db.Name, "err", err)
			_ = s.store.FinishBackup(ctx, b.ID, "completed", "local only: S3 upload failed: "+err.Error(), rep.StdoutBytes)
		} else {
			_ = s.store.SetBackupS3Key(ctx, b.ID, key)
			b.S3Key = key
		}
	}
	// Retention: keep the 7 most recent backups.
	if old, err := s.store.OldBackups(ctx, db.ID, 7); err == nil {
		for _, o := range old {
			if o.NodeID != nil {
				_ = s.requestAgent(ctx, *o.NodeID, proto.CmdRemoveFiles, proto.RemoveFilesRequest{Paths: []string{o.Path}}, &proto.GenericReply{}, time.Minute)
			}
			if o.S3Key != "" && s.s3.Enabled() {
				_ = s.s3.Delete(o.S3Key)
			}
			_ = s.store.DeleteBackup(ctx, o.ID)
		}
	}
	return b, nil
}

// restoreBackup restores a backup into the server db points at. foreign: the dump came from another
// server (a move), see databases.Restore.
func (s *Server) restoreBackup(ctx context.Context, db *store.Database, b *store.Backup, foreign bool) error {
	if b.Status != "completed" {
		return errors.New("backup is not complete")
	}
	c, srv, err := s.dbCreds(ctx, db)
	if err != nil {
		return err
	}
	in, err := s.firstInstance(ctx, srv.ID)
	if err != nil {
		return err
	}
	if b.NodeID == nil || *b.NodeID != in.NodeID {
		if b.S3Key == "" || !s.s3.Enabled() {
			return errors.New("backup file is stored on a different node than the database server (configure S3 for off-site backups)")
		}
		// Fetch the off-site copy onto the database server's node.
		if err := s.transfer(ctx, in.NodeID, http.MethodGet, b.S3Key, b.Path); err != nil {
			return fmt.Errorf("download backup from S3: %w", err)
		}
	}
	cmd, err := databases.Restore(db.Engine, db.Mode == "shared", c, foreign)
	if err != nil {
		return err
	}
	return s.execOnService(ctx, srv.ID, cmd, b.Path, "", 1)
}

func (s *Server) runScheduledBackups(ctx context.Context) {
	dbs, err := s.store.AllDatabases(ctx)
	if err != nil {
		return
	}
	for _, db := range dbs {
		if db.Status != "ready" || db.BackupScheduleHours <= 0 || db.Engine == databases.Redis && db.Mode == "shared" {
			continue
		}
		due := db.LastBackupAt == nil && time.Since(db.CreatedAt) > time.Hour ||
			db.LastBackupAt != nil && time.Since(*db.LastBackupAt) > time.Duration(db.BackupScheduleHours)*time.Hour
		if !due {
			continue
		}
		if _, err := s.backupDatabase(ctx, &db); err != nil {
			slog.Warn("scheduled backup failed", "db", db.Name, "err", err)
			if p, perr := s.store.ProjectByID(ctx, db.ProjectID); perr == nil {
				s.notify(ctx, p.TeamID, "backup.failed", fmt.Sprintf("Backup of database %s/%s failed: %s", p.Slug, db.Name, truncate(err.Error(), 200)), nil)
			}
			_ = s.store.TouchDatabaseBackup(ctx, db.ID) // do not retry every minute
		}
	}
}

// transfer asks an agent to move a file to or from object storage with a presigned URL.
func (s *Server) transfer(ctx context.Context, nodeID, method, key, path string) error {
	u, err := s.s3.Presign(method, key, 2*time.Hour, time.Now())
	if err != nil {
		return err
	}
	_, err = s.requestTransfer(ctx, nodeID, proto.TransferRequest{Method: method, URL: u, Path: path}, 2*time.Hour)
	return err
}

func (s *Server) requestTransfer(ctx context.Context, nodeID string, req proto.TransferRequest, timeout time.Duration) ([]string, error) {
	var rep proto.TransferReply
	if err := s.requestAgent(ctx, nodeID, proto.CmdTransfer, req, &rep, timeout); err != nil {
		return nil, err
	}
	if rep.Error != "" {
		return nil, errors.New(rep.Error)
	}
	return rep.ETags, nil
}

const (
	multipartMin  = 64 << 20 // files above this are uploaded in parts
	multipartPart = 64 << 20
)

// uploadLarge uploads a file of size bytes: one PUT up to multipartMin, else a
// multipart upload whose parts the agent PUTs one by one; the ETags it returns
// are handed to CompleteMultipart in part order.
func (s *Server) uploadLarge(ctx context.Context, nodeID, key, path string, size int64) error {
	if size <= multipartMin {
		return s.transfer(ctx, nodeID, http.MethodPut, key, path)
	}
	id, err := s.s3.CreateMultipart(key)
	if err != nil {
		return err
	}
	n := int((size + multipartPart - 1) / multipartPart)
	// The agent uploads the parts one at a time: sign for as long as the request may run.
	const timeout = 6 * time.Hour
	urls := make([]string, n)
	for i := range urls {
		if urls[i], err = s.s3.PresignPart(key, id, i+1, timeout); err != nil {
			_ = s.s3.AbortMultipart(key, id)
			return err
		}
	}
	etags, err := s.requestTransfer(ctx, nodeID, proto.TransferRequest{Method: http.MethodPut, Path: path, PartURLs: urls, PartSize: multipartPart}, timeout)
	if err == nil && len(etags) != n {
		err = fmt.Errorf("the agent returned %d ETags for %d parts (agent too old?)", len(etags), n)
	}
	if err == nil {
		parts := make([]s3.Part, n)
		for i, e := range etags {
			parts[i] = s3.Part{N: i + 1, ETag: e}
		}
		err = s.s3.CompleteMultipart(key, id, parts)
	}
	if err != nil {
		_ = s.s3.AbortMultipart(key, id)
	}
	return err
}
