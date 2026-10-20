package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
	"github.com/thesunbg/paas/internal/store"
)

// A "mysql" database made before the split lives on a MariaDB shared server. After migration 019 it
// is engine "mariadb" on that same server with the same root password, and asking for engine
// "mysql" makes a new, separate server.
func TestSplitMariaDBMigration(t *testing.T) {
	s := newTestServerBus(t)
	ctx := context.Background()
	team, err := s.store.CreateTeam(ctx, "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sys, err := s.store.CreateProject(ctx, team.ID, "system", systemProjectSlug)
	if err != nil {
		t.Fatal(err)
	}
	s.systemProjectID = sys.ID
	app, err := s.store.CreateProject(ctx, team.ID, "app", "app")
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.store.UpsertNodeLegacy(ctx, &proto.RegisterRequest{Name: "a", Address: "10.0.0.1", Roles: []string{"worker"}, CPUCores: 2, MemoryMB: 2048, Capabilities: map[string]string{}}, 16)
	if err != nil {
		t.Fatal(err)
	}

	// The state the old binary left behind.
	spec, err := databases.ServerSpec(databases.MariaDB, "11.8", databases.Presets[2], true, databases.Credentials{RootPass: "rootpw"})
	if err != nil {
		t.Fatal(err)
	}
	spec.Language = "mysql"
	old, err := s.store.CreateService(ctx, &store.Service{ProjectID: sys.ID, Name: "Shared mysql 11.8", Slug: "shared-mysql", Type: model.TypeDatabase,
		Source: model.ServiceSource{Kind: "image", Image: spec.Image}, Spec: spec, WebhookSecret: "w"})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := s.store.CreateDeployment(ctx, &store.Deployment{ServiceID: old.ID, Status: model.DeployActive, Trigger: "test", Image: spec.Image})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetServiceCurrentDeployment(ctx, old.ID, dep.ID, model.StatusRunning); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.CreateInstance(ctx, &store.Instance{ServiceID: old.ID, DeploymentID: &dep.ID, NodeID: node.ID, ContainerID: "c1", Port: 3306, Status: model.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetSetting(ctx, "shared_mysql_root", s.box.MustEncrypt("rootpw")); err != nil {
		t.Fatal(err)
	}
	db, err := s.store.CreateDatabase(ctx, &store.Database{ProjectID: app.ID, Name: "wp", Engine: "mysql", Version: "11.8", Mode: "shared",
		DBName: "wp", Username: "wp", PasswordEnc: s.box.MustEncrypt("pw"), SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetDatabaseServer(ctx, db.ID, old.ID); err != nil {
		t.Fatal(err)
	}
	other, err := s.store.CreateDatabase(ctx, &store.Database{ProjectID: app.ID, Name: "pg", Engine: "postgres", Version: "17", Mode: "dedicated",
		DBName: "pg", Username: "pg", PasswordEnc: s.box.MustEncrypt("pw"), SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile("../store/migrations/019_split_mariadb.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.DB.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}

	got, err := s.store.DatabaseByID(ctx, db.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Engine != "mariadb" {
		t.Fatalf("engine %q", got.Engine)
	}
	if o, _ := s.store.DatabaseByID(ctx, other.ID); o.Engine != "postgres" {
		t.Fatalf("postgres database touched: %q", o.Engine)
	}
	srv, err := s.store.ServiceByID(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Slug != "shared-mariadb" || srv.Name != "Shared mariadb 11.8" || srv.Spec.Language != "mariadb" || srv.Version != old.Version+1 {
		t.Fatalf("service after migration: slug %q name %q language %q version %d", srv.Slug, srv.Name, srv.Spec.Language, srv.Version)
	}
	if srv.Status != model.StatusRunning || srv.Spec.Image != spec.Image {
		t.Fatalf("migration restarted or re-imaged the server: %s %s", srv.Status, srv.Spec.Image)
	}
	if _, err := s.store.GetSetting(ctx, "shared_mysql_root"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old root setting still there: %v", err)
	}

	// The old database keeps working on the old server.
	same, err := s.ensureSharedServer(ctx, databases.MariaDB, "11.8")
	if err != nil {
		t.Fatal(err)
	}
	if same.ID != old.ID {
		t.Fatalf("ensureSharedServer made a second server %s (%s), want %s", same.ID, same.Slug, old.ID)
	}
	c, srv2, err := s.dbCreds(ctx, got)
	if err != nil {
		t.Fatal(err)
	}
	if srv2.ID != old.ID || c.RootPass != "rootpw" || !strings.HasPrefix(c.Host, "shared-mariadb.") {
		t.Fatalf("creds %+v on %s", c, srv2.Slug)
	}
	bak, _, _ := databases.Backup(got.Engine, true, c)
	res, _ := databases.Restore(got.Engine, true, c)
	ro, _ := databases.SetReadOnly(got.Engine, true, c, true)
	for name, cmd := range map[string]databases.Command{"backup": bak, "restore": res, "readonly": ro} {
		if cmd.Env[0] != "MYSQL_PWD=rootpw" || !strings.HasPrefix(cmd.Cmd[0], "mariadb") {
			t.Errorf("%s: %v %v", name, cmd.Cmd, cmd.Env)
		}
	}

	// "mysql" is a different engine now: its own server with its own image and root password
	// (the deployment never completes in a test, so give up waiting for it).
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = s.ensureSharedServer(wctx, databases.MySQL, "8.4")
	all, err := s.store.ServicesForProject(ctx, sys.ID)
	if err != nil {
		t.Fatal(err)
	}
	bySlug := map[string]store.Service{}
	for _, sv := range all {
		bySlug[sv.Slug] = sv
	}
	my, ok := bySlug["shared-mysql"]
	if !ok || len(bySlug) != 2 {
		t.Fatalf("services after ensureSharedServer(mysql): %v", bySlug)
	}
	if my.ID == old.ID || my.Spec.Image != "docker.io/library/mysql:8.4" || bySlug["shared-mariadb"].Spec.Image != spec.Image {
		t.Fatalf("shared-mysql %s image %s; shared-mariadb image %s", my.ID, my.Spec.Image, bySlug["shared-mariadb"].Spec.Image)
	}
	if _, err := s.store.GetSetting(ctx, "shared_mysql_root"); err != nil {
		t.Fatalf("no new mysql root password: %v", err)
	}
	maria, _ := s.sharedRootPassword(ctx, databases.MariaDB)
	if mysqlRoot, _ := s.sharedRootPassword(ctx, databases.MySQL); maria != "rootpw" || mysqlRoot == maria {
		t.Fatalf("root passwords mariadb=%q mysql=%q", maria, mysqlRoot)
	}
}
