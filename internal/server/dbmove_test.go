package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

func init() { linkedStartWait = 3 * time.Second }

// linked adds a running web service linked to db (env DATABASE_URL), with a running instance.
func (e *minorEnv) linked(t *testing.T, db *store.Database, slug string) *store.Service {
	t.Helper()
	ctx := context.Background()
	s := e.s
	svc, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: e.proj.ID, Name: slug, Slug: slug, Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "image", Image: "app:1"}, NodeID: &e.node.ID,
		Spec: model.ServiceSpec{Image: "app:1", Replicas: 1, Port: 8080, QoS: model.QoSBurstable, Resources: model.Resources{MemoryLimitMB: 64, MemoryRequestMB: 32},
			HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: svc.ID, Status: model.DeployActive, Trigger: "system", Image: "app:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetServiceCurrentDeployment(ctx, svc.ID, d.ID, model.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: svc.ID, DeploymentID: &d.ID, NodeID: e.node.ID, ContainerID: slug + "-c", Port: 8080, Status: model.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.LinkDatabase(ctx, svc.ID, db.ID, "DATABASE_URL"); err != nil {
		t.Fatal(err)
	}
	return svc
}

// shared makes the system project and a running shared postgres 17 server for the databases of the test.
func (e *minorEnv) sharedServer(t *testing.T) *store.Service {
	t.Helper()
	ctx := context.Background()
	s := e.s
	sys, err := s.store.CreateProject(ctx, e.team.ID, "system", systemProjectSlug)
	if err != nil {
		t.Fatal(err)
	}
	s.systemProjectID = sys.ID
	pinned := "postgres:17@sha256:old"
	srv, err := s.store.CreateService(ctx, &store.Service{
		ProjectID: sys.ID, Name: "Shared postgres 17", Slug: "shared-postgres", Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: pinned}, NodeID: &e.node.ID,
		Spec: model.ServiceSpec{Image: pinned, Language: "postgres", Replicas: 1, Port: 5432, QoS: model.QoSBurstable,
			Volumes:     []model.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}},
			Resources:   model.Resources{MemoryLimitMB: 256, MemoryRequestMB: 128},
			HealthCheck: model.HealthCheck{Type: "tcp", TimeoutSec: 5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: srv.ID, Status: model.DeployActive, Trigger: "system", Image: "docker.io/library/" + pinned})
	_ = s.store.SetServiceCurrentDeployment(ctx, srv.ID, d.ID, model.StatusRunning)
	if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: srv.ID, DeploymentID: &d.ID, NodeID: e.node.ID, ContainerID: "shared17-c", Port: 5432, Status: model.InstRunning}); err != nil {
		t.Fatal(err)
	}
	return srv
}

func (e *minorEnv) version(t *testing.T, who string, db *store.Database, body any) int {
	t.Helper()
	return do(t, e.h, "POST", "/api/v1/databases/"+db.ID+"/version", e.tok[who], body).Code
}

func (e *minorEnv) upgradeTo(t *testing.T, db *store.Database, v string) *store.Database {
	t.Helper()
	if code := e.version(t, "tadmin", db, map[string]any{"version": v, "confirm_downtime": true}); code != 202 {
		t.Fatalf("POST version = %d, want 202", code)
	}
	return e.settle(t, db)
}

func (e *minorEnv) hasService(t *testing.T, id string) bool {
	t.Helper()
	_, err := e.s.store.ServiceByID(context.Background(), id)
	return err == nil
}

func (e *minorEnv) serviceBySlug(t *testing.T, slug string) *store.Service {
	t.Helper()
	for _, pid := range []string{e.proj.ID, e.s.systemProjectID} {
		svcs, _ := e.s.store.ServicesForProject(context.Background(), pid)
		for i := range svcs {
			if svcs[i].Slug == slug {
				return &svcs[i]
			}
		}
	}
	return nil
}

// at returns the index of the first timeline entry containing all of parts, or -1.
func at(tl []string, parts ...string) int {
	for i, l := range tl {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return i
		}
	}
	return -1
}

func requireBefore(t *testing.T, tl []string, a, b []string) {
	t.Helper()
	i, j := at(tl, a...), at(tl, b...)
	if i < 0 || j < 0 || i >= j {
		t.Fatalf("want %v (at %d) before %v (at %d) in:\n%s", a, i, b, j, strings.Join(tl, "\n"))
	}
}

func inventoryHook(src, dst string) func(*proto.ExecRequest) *proto.ExecReply {
	return func(r *proto.ExecRequest) *proto.ExecReply {
		if strings.Contains(r.Stdin, "pg_stat_user_tables") {
			if strings.Contains(r.ContainerID, "18") {
				return &proto.ExecReply{Stdout: dst}
			}
			return &proto.ExecReply{Stdout: src}
		}
		return nil
	}
}

const inv = "T|public.a\nT|public.b\nC|public.a|5\nC|public.b|7\n"

func TestMajorUpgradeDedicated(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	db, old := e.database(t, "d1", "dedicated", nil)
	web := e.linked(t, db, "web")
	if err := e.s.store.CreateDBUser(ctx, store.DBUser{DatabaseID: db.ID, Username: "d1_ro", Role: "readonly", PasswordEnc: e.s.box.MustEncrypt("ropass")}); err != nil {
		t.Fatal(err)
	}
	e.agent.execHook = inventoryHook(inv, inv)

	got := e.upgradeTo(t, db, "18")
	if got.Status != "ready" || got.Error != "" {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
	if got.Version != "18" || got.ServerServiceID == nil || *got.ServerServiceID == old.ID || got.MoveState != nil {
		t.Fatalf("db = version %s server %v move_state %+v", got.Version, got.ServerServiceID, got.MoveState)
	}
	if e.hasService(t, old.ID) {
		t.Fatal("the old server was not removed")
	}
	nsrv, err := e.s.store.ServiceByID(ctx, *got.ServerServiceID)
	if err != nil || nsrv.Slug != "d1-v18" || !strings.HasPrefix(nsrv.Spec.Image, "docker.io/library/postgres:18") {
		t.Fatalf("new server = %+v err %v", nsrv, err)
	}
	if bs, _ := e.s.store.BackupsForDatabase(ctx, db.ID); len(bs) != 1 || !bs[0].Pinned || bs[0].SizeBytes <= 0 {
		t.Fatalf("backups = %+v, want one pinned non-empty", bs)
	}

	tl := e.agent.tlCopy()
	// Frozen: linked service stopped and database read-only, both before the dump.
	requireBefore(t, tl, []string{"stop web-c"}, []string{"pg_dump"})
	requireBefore(t, tl, []string{"d1-c", "default_transaction_read_only = on"}, []string{"pg_dump"})
	// The dump is restored without ACLs into the NEW server, the extra user replayed and checked there.
	requireBefore(t, tl, []string{"pg_dump"}, []string{"-v18", "pg_restore", "--no-acl"})
	requireBefore(t, tl, []string{"-v18", "pg_restore"}, []string{"-v18", "CREATE ROLE", "d1_ro"})
	requireBefore(t, tl, []string{"-v18", "CREATE ROLE"}, []string{"-v18", "PGPASSWORD=ropass"})
	// Linked service starts only after the flip, with the URL of the new server; the old server goes last.
	requireBefore(t, tl, []string{"-v18", "PGPASSWORD=ropass"}, []string{"run web-", "app:1"})
	requireBefore(t, tl, []string{"run web-", "app:1"}, []string{"stop d1-c"})
	var url string
	for _, r := range e.agent.runs {
		if r.ServiceID == web.ID {
			url = r.Env["DATABASE_URL"]
		}
	}
	if !strings.Contains(url, "d1-v18.p.internal") {
		t.Fatalf("linked service restarted with DATABASE_URL %q, want the new server", url)
	}
	if fresh, _ := e.s.store.ServiceByID(ctx, web.ID); fresh.Status != model.StatusRunning {
		t.Fatalf("linked service status %s", fresh.Status)
	}
	// Nothing was touched on the old container after the freeze but the dump and the readonly switch.
	for _, l := range tl {
		if strings.Contains(l, "exec d1-c") && strings.Contains(l, "DROP") {
			t.Fatalf("dedicated old server must go with its volume, not be dropped logically: %s", l)
		}
	}
}

func TestMajorUpgradeShared(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	shared := e.sharedServer(t)
	db, _ := e.database(t, "s1", "shared", shared)
	sibling, _ := e.database(t, "s2", "shared", shared)
	web := e.linked(t, db, "web")
	e.agent.execHook = inventoryHook(inv, inv)

	got := e.upgradeTo(t, db, "18")
	if got.Status != "ready" || got.Error != "" {
		t.Fatalf("status %q error %q", got.Status, got.Error)
	}
	nsrv := e.serviceBySlug(t, "shared-postgres-18")
	if nsrv == nil || got.ServerServiceID == nil || *got.ServerServiceID != nsrv.ID || got.Version != "18" || got.Mode != "shared" {
		t.Fatalf("db = %+v, shared 18 server = %+v", got, nsrv)
	}
	if !e.hasService(t, shared.ID) {
		t.Fatal("the shared server of the other tenants must stay")
	}
	if sib, _ := e.s.store.DatabaseByID(ctx, sibling.ID); sib.Version != "17" || *sib.ServerServiceID != shared.ID {
		t.Fatalf("sibling moved: %+v", sib)
	}
	tl := e.agent.tlCopy()
	// Logical database and role created on the target before the restore; dropped on the old server last.
	requireBefore(t, tl, []string{"pg_dump"}, []string{"-18", "CREATE ROLE"})
	requireBefore(t, tl, []string{"-18", "CREATE ROLE"}, []string{"-18", "pg_restore", "--role="})
	requireBefore(t, tl, []string{"-18", "pg_restore"}, []string{"run web-"})
	requireBefore(t, tl, []string{"run web-"}, []string{"exec shared17-c", "DROP DATABASE IF EXISTS"})
	if i := at(tl, "exec shared17-c", "DROP DATABASE IF EXISTS \"s1\""); i < 0 {
		t.Fatalf("old logical database not dropped:\n%s", strings.Join(tl, "\n"))
	}
	if fresh, _ := e.s.store.ServiceByID(ctx, web.ID); fresh.Status != model.StatusRunning {
		t.Fatalf("linked service status %s", fresh.Status)
	}
}

// A failure at any step before the flip leaves the old server as the database.
func TestMajorUpgradeFailureBeforeFlip(t *testing.T) {
	cases := []struct {
		name   string
		shared bool
		hook   func(*proto.ExecRequest) *proto.ExecReply
		want   string
	}{
		{"restore killed, dedicated", false, func(r *proto.ExecRequest) *proto.ExecReply {
			if strings.Contains(strings.Join(r.Cmd, " "), "pg_restore") {
				return &proto.ExecReply{ExitCode: 137, Stderr: "killed"}
			}
			return nil
		}, "restore"},
		{"restore killed, shared", true, func(r *proto.ExecRequest) *proto.ExecReply {
			if strings.Contains(strings.Join(r.Cmd, " "), "pg_restore") {
				return &proto.ExecReply{ExitCode: 137, Stderr: "killed"}
			}
			return nil
		}, "restore"},
		{"row counts differ, dedicated", false, inventoryHook(inv, strings.Replace(inv, "|5", "|4", 1)), "rows"},
		{"table missing, shared", true, inventoryHook(inv, "T|public.a\nC|public.a|5\n"), "tables differ"},
		{"extension missing, dedicated", false, func(r *proto.ExecRequest) *proto.ExecReply {
			if strings.Contains(strings.Join(r.Cmd, " "), "pg_extension") {
				return &proto.ExecReply{Stdout: "postgis\n"}
			}
			if strings.Contains(strings.Join(r.Cmd, " "), "pg_available_extensions") {
				return &proto.ExecReply{Stdout: "pg_trgm\n"}
			}
			return nil
		}, "postgis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMinorEnv(t)
			ctx := context.Background()
			var srv *store.Service
			mode := "dedicated"
			if tc.shared {
				srv, mode = e.sharedServer(t), "shared"
			}
			db, old := e.database(t, "d1", mode, srv)
			web := e.linked(t, db, "web")
			if err := e.s.store.CreateDBUser(ctx, store.DBUser{DatabaseID: db.ID, Username: "d1_ro", Role: "readonly", PasswordEnc: e.s.box.MustEncrypt("ropass")}); err != nil {
				t.Fatal(err)
			}
			e.agent.execHook = tc.hook

			got := e.upgradeTo(t, db, "18")
			if got.Status != "ready" || !strings.Contains(got.Error, tc.want) || !strings.Contains(got.Error, "failed") {
				t.Fatalf("status %q error %q, want ready + an error mentioning %q", got.Status, got.Error, tc.want)
			}
			if got.ServerServiceID == nil || *got.ServerServiceID != old.ID || got.Version != "17" || got.Mode != mode || got.MoveState != nil {
				t.Fatalf("the pointer moved: %+v", got)
			}
			if !e.hasService(t, old.ID) {
				t.Fatal("the old server is gone")
			}
			// Everything the failed move created is gone, the old database writable and the app back.
			if e.serviceBySlug(t, "d1-v18") != nil {
				t.Fatal("the new dedicated server was left behind")
			}
			tl := e.agent.tlCopy()
			if i := at(tl, "default_transaction_read_only = off"); i < 0 {
				t.Fatalf("writes were not re-enabled:\n%s", strings.Join(tl, "\n"))
			}
			if tc.shared && strings.Contains(tc.name, "restore") {
				if at(tl, "-18", "DROP DATABASE IF EXISTS") < 0 {
					t.Fatalf("the partial logical database on the target was not dropped:\n%s", strings.Join(tl, "\n"))
				}
				if at(tl, "exec shared17-c", "DROP DATABASE") >= 0 {
					t.Fatal("the old logical database was dropped")
				}
			}
			if fresh, _ := e.s.store.ServiceByID(ctx, web.ID); fresh.Status != model.StatusRunning {
				t.Fatalf("linked service status %s", fresh.Status)
			}
			requireBefore(t, tl, []string{"stop web-c"}, []string{"run web-"})
			if bs, _ := e.s.store.BackupsForDatabase(ctx, db.ID); tc.want != "postgis" && (len(bs) != 1 || !bs[0].Pinned) {
				t.Fatalf("the backup taken after the freeze must stay pinned: %+v", bs)
			}
		})
	}
}

// resumeProvisioning on both sides of the flip.
func TestResumeMove(t *testing.T) {
	setup := func(t *testing.T) (*minorEnv, *store.Database, *store.Service, *store.Service, *store.Service) {
		e := newMinorEnv(t)
		ctx := context.Background()
		db, old := e.database(t, "d1", "dedicated", nil)
		web := e.linked(t, db, "web")
		e.s.stopService(ctx, web)
		pinned := "postgres:18"
		nsrv, err := e.s.store.CreateService(ctx, &store.Service{
			ProjectID: e.proj.ID, Name: "d1 v18", Slug: "d1-v18", Type: model.TypeDatabase,
			Source: model.ServiceSource{Kind: "image", Image: pinned}, NodeID: &e.node.ID,
			Spec: model.ServiceSpec{Image: pinned, Language: "postgres", Replicas: 1, Port: 5432}})
		if err != nil {
			t.Fatal(err)
		}
		// resume works on a restarted control plane: nothing of the job's goroutine is left.
		if err := e.s.store.SetDatabaseStatus(ctx, db.ID, "upgrading", ""); err != nil {
			t.Fatal(err)
		}
		return e, db, old, nsrv, web
	}
	waitCleared := func(t *testing.T, e *minorEnv, db *store.Database) *store.Database {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			got, _ := e.s.store.DatabaseByID(context.Background(), db.ID)
			if got.MoveState == nil {
				return got
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("move_state was not cleared")
		return nil
	}

	t.Run("before the flip: undone", func(t *testing.T) {
		e, db, old, nsrv, web := setup(t)
		ctx := context.Background()
		if err := e.s.store.SetMoveState(ctx, db.ID, &store.MoveState{Phase: store.MovePrepare, OldService: old.ID, NewService: nsrv.ID,
			TargetVersion: "18", TargetDedicated: true, Stopped: []string{web.ID}}); err != nil {
			t.Fatal(err)
		}
		e.s.resumeProvisioning(ctx)
		got := waitCleared(t, e, db)
		if got.Status != "ready" || !strings.Contains(got.Error, "interrupted") {
			t.Fatalf("status %q error %q", got.Status, got.Error)
		}
		if *got.ServerServiceID != old.ID || got.Version != "17" {
			t.Fatalf("the pointer moved: %+v", got)
		}
		if e.hasService(t, nsrv.ID) || !e.hasService(t, old.ID) {
			t.Fatal("the new server must be deleted and the old one kept")
		}
		if fresh, _ := e.s.store.ServiceByID(ctx, web.ID); fresh.Status != model.StatusRunning {
			t.Fatalf("linked service status %s", fresh.Status)
		}
		if at(e.agent.tlCopy(), "default_transaction_read_only = off") < 0 {
			t.Fatal("writes were not re-enabled")
		}
	})

	t.Run("after the flip: finished", func(t *testing.T) {
		e, db, old, nsrv, web := setup(t)
		ctx := context.Background()
		st := store.MoveState{Phase: store.MovePrepare, OldService: old.ID, NewService: nsrv.ID, TargetVersion: "18", TargetDedicated: true, Stopped: []string{web.ID}}
		if ok, err := e.s.store.FlipDatabase(ctx, db.ID, old.ID, nsrv.ID, "18", "dedicated", st); err != nil || !ok {
			t.Fatalf("flip: %v %v", ok, err)
		}
		// The compare-and-swap is one-shot: a second flip from the old server must not apply.
		if ok, _ := e.s.store.FlipDatabase(ctx, db.ID, old.ID, nsrv.ID, "18", "dedicated", st); ok {
			t.Fatal("a stale flip applied")
		}
		e.s.resumeProvisioning(ctx)
		got := waitCleared(t, e, db)
		if got.Status != "ready" || got.Error != "" || *got.ServerServiceID != nsrv.ID || got.Version != "18" {
			t.Fatalf("db = %+v", got)
		}
		if e.hasService(t, old.ID) || !e.hasService(t, nsrv.ID) {
			t.Fatal("the old server must be deleted and the new one kept")
		}
		if fresh, _ := e.s.store.ServiceByID(ctx, web.ID); fresh.Status != model.StatusRunning {
			t.Fatalf("linked service status %s", fresh.Status)
		}
	})
}

// Every request and loop that would touch a database refuses or skips it while it is `upgrading`.
func TestUpgradingBlocksOtherOperations(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	s := e.s
	ded, _ := e.database(t, "d1", "dedicated", nil)
	sharedSrv := e.sharedServer(t)
	sh, _ := e.database(t, "s1", "shared", sharedSrv)
	b, err := s.store.CreateBackup(ctx, ded.ID, e.node.ID, "backups/x/a.dump")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.store.FinishBackup(ctx, b.ID, "completed", "", 10)
	for _, d := range []*store.Database{ded, sh} {
		if err := s.store.SetDatabaseStatus(ctx, d.ID, "upgrading", ""); err != nil {
			t.Fatal(err)
		}
	}
	id := "/api/v1/databases/" + ded.ID
	reqs := []struct {
		name, method, path string
		body               any
		codes              []int
	}{
		{"enable HA", "POST", id + "/ha", map[string]any{"enabled": true}, []int{409}},
		{"enable PITR", "POST", id + "/pitr", map[string]any{"enabled": true, "retention_days": 3}, []int{409}},
		{"import", "POST", id + "/import", map[string]any{"source_url": "postgres://u:p@h/db"}, []int{409}},
		{"resize", "PUT", id + "/size", map[string]any{"size": "small"}, []int{409}},
		{"manual backup", "POST", id + "/backups", nil, []int{400, 409}},
		{"restore backup", "POST", id + "/backups/" + b.ID + "/restore", nil, []int{409}},
		{"update-minor", "POST", id + "/update-minor", nil, []int{409}},
		{"major upgrade", "POST", id + "/version", map[string]any{"version": "18", "confirm_downtime": true}, []int{409}},
		{"shared to dedicated", "POST", "/api/v1/databases/" + sh.ID + "/upgrade", nil, []int{400, 409}},
	}
	for _, r := range reqs {
		got := do(t, e.h, r.method, r.path, e.tok["root"], r.body).Code
		ok := false
		for _, c := range r.codes {
			ok = ok || got == c
		}
		if !ok {
			t.Errorf("%s while upgrading = %d, want one of %v", r.name, got, r.codes)
		}
	}

	// Loops: scheduled backups and the HA tick.
	if _, err := s.store.Q(ctx).Exec(ctx, `UPDATE databases SET backup_schedule_hours=1, created_at=now()-interval '3 days'`); err != nil {
		t.Fatal(err)
	}
	s.runScheduledBackups(ctx)
	rep, _ := s.store.ServiceByID(ctx, sharedSrv.ID) // any other service stands in for a standby
	if _, err := s.store.UpdateDatabaseOptions(ctx, ded.ID, func(o *store.DBOptions) error { o.HA, o.ReplicaServiceID = true, rep.ID; return nil }); err != nil {
		t.Fatal(err)
	}
	s.haTick(ctx)
	if ops := e.agent.opsCopy(); len(ops) != 0 {
		t.Fatalf("loops reached the agent while the database was upgrading: %v", ops)
	}
	// The same loops do act on a ready database, so the silence above means something.
	if err := s.store.SetDatabaseStatus(ctx, ded.ID, "ready", ""); err != nil {
		t.Fatal(err)
	}
	s.runScheduledBackups(ctx)
	if ops := e.agent.opsCopy(); len(ops) == 0 {
		t.Fatal("scheduled backups did not run for a ready database: the check above proves nothing")
	}
}

func TestMajorUpgradeRequestChecks(t *testing.T) {
	e := newMinorEnv(t)
	ctx := context.Background()
	ok := map[string]any{"version": "18", "confirm_downtime": true}
	db, _ := e.database(t, "d1", "dedicated", nil)

	for _, who := range []string{"dev", "viewer"} {
		if code := e.version(t, who, db, ok); code != 403 {
			t.Errorf("%s = %d, want 403", who, code)
		}
	}
	if code := e.version(t, "tadmin", db, map[string]any{"version": "18"}); code != 400 {
		t.Errorf("without confirm_downtime = %d, want 400", code)
	}
	for _, v := range []string{"17", "16", "99", "18@sha256:abc", "../x", ""} {
		if code := e.version(t, "tadmin", db, map[string]any{"version": v, "confirm_downtime": true}); code != 400 {
			t.Errorf("version %q = %d, want 400 (not an upgrade in the catalog)", v, code)
		}
	}
	if got, _ := e.s.store.DatabaseByID(ctx, db.ID); got.Status != "ready" {
		t.Fatalf("a refused request changed the status to %q", got.Status)
	}

	// HA, PITR, pooler: turn off first.
	for name, f := range map[string]func(*store.DBOptions){
		"HA": func(o *store.DBOptions) { o.HA = true }, "PITR": func(o *store.DBOptions) { o.PITR = true }, "pooler": func(o *store.DBOptions) { o.Pooler = true },
	} {
		d, _ := e.database(t, "x"+strings.ToLower(name), "dedicated", nil)
		if _, err := e.s.store.UpdateDatabaseOptions(ctx, d.ID, func(o *store.DBOptions) error { f(o); return nil }); err != nil {
			t.Fatal(err)
		}
		if code := e.version(t, "tadmin", d, ok); code != 409 {
			t.Errorf("%s = %d, want 409", name, code)
		}
	}

	// Redis has no restore.
	rd, err := e.s.store.CreateDatabase(ctx, &store.Database{ProjectID: e.proj.ID, Name: "r1", Engine: "redis", Version: "7.2", Mode: "dedicated",
		DBName: "r1", Username: "r1", PasswordEnc: e.s.box.MustEncrypt("pw"), SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.s.store.SetDatabaseStatus(ctx, rd.ID, "ready", "")
	if code := e.version(t, "tadmin", rd, map[string]any{"version": "7.4", "confirm_downtime": true}); code != 409 {
		t.Errorf("redis = %d, want 409", code)
	}

	// Not ready.
	if err := e.s.store.SetDatabaseStatus(ctx, db.ID, "failed", "x"); err != nil {
		t.Fatal(err)
	}
	if code := e.version(t, "tadmin", db, ok); code != 409 {
		t.Errorf("not ready = %d, want 409", code)
	}
	if ops := e.agent.opsCopy(); len(ops) != 0 {
		t.Errorf("refused requests reached the agent: %v", ops)
	}
}
