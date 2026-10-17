package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/thesunbg/paas/internal/model"
)

// testStore gives the test its own migrated database; skips unless PAAS_TEST_DATABASE_URL is set.
func testStore(t *testing.T) *Store {
	t.Helper()
	adminURL := os.Getenv("PAAS_TEST_DATABASE_URL")
	if adminURL == "" {
		t.Skip("PAAS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	defer admin.Close(ctx)
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "paas_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	u, _ := url.Parse(adminURL)
	u.Path = "/" + name
	st, err := Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		if c, err := pgx.Connect(ctx, adminURL); err == nil {
			defer c.Close(ctx)
			_, _ = c.Exec(ctx, `DROP DATABASE `+name+` WITH (FORCE)`)
		}
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func seedProject(t *testing.T, st *Store) *Project {
	t.Helper()
	team, err := st.CreateTeam(context.Background(), "t", "t", "", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(context.Background(), team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func seedService(t *testing.T, st *Store, p *Project) *Service {
	t.Helper()
	svc, err := st.CreateService(context.Background(), &Service{
		ProjectID: p.ID, Name: "web", Slug: "web", Type: model.TypeWeb,
		Source: model.ServiceSource{Kind: "git", Repo: "https://example.invalid/r.git", Branch: "main"},
		Spec:   model.ServiceSpec{Replicas: 1, Port: 8080},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// A build that loaded the service minutes ago must not undo a replica change made meanwhile.
func TestUpdateServiceKeepsConcurrentChange(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := seedService(t, st, seedProject(t, st))
	if svc.Version != 1 {
		t.Fatalf("initial version = %d", svc.Version)
	}
	calls := 0
	got, err := st.UpdateService(ctx, svc.ID, func(f *Service) error {
		calls++
		if calls == 1 { // the user scales while the "build" is in flight
			if _, err := st.UpdateService(ctx, svc.ID, func(g *Service) error { g.Spec.Replicas = 3; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		f.Spec.Language = "go"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("closure ran %d times, want 2", calls)
	}
	row, err := st.ServiceByID(ctx, svc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Spec.Replicas != 3 || row.Spec.Language != "go" {
		t.Fatalf("row spec = replicas %d language %q, want 3 and go", row.Spec.Replicas, row.Spec.Language)
	}
	if row.Version != 3 || got.Version != 3 {
		t.Fatalf("version = %d (returned %d), want 3", row.Version, got.Version)
	}
}

func TestUpdateServiceConflictAndAbort(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	svc := seedService(t, st, seedProject(t, st))
	calls := 0
	_, err := st.UpdateService(ctx, svc.ID, func(f *Service) error {
		calls++
		_, err := st.DB.Exec(ctx, `UPDATE services SET version=version+1 WHERE id=$1`, svc.ID)
		return err
	})
	if !errors.Is(err, ErrConflict) || calls != casAttempts {
		t.Fatalf("err = %v after %d calls, want ErrConflict after %d", err, calls, casAttempts)
	}
	stop := errors.New("stop")
	before, _ := st.ServiceByID(ctx, svc.ID)
	if _, err := st.UpdateService(ctx, svc.ID, func(f *Service) error { f.Spec.Replicas = 9; return stop }); err != stop {
		t.Fatalf("err = %v, want the closure's error", err)
	}
	after, _ := st.ServiceByID(ctx, svc.ID)
	if after.Version != before.Version || after.Spec.Replicas != before.Spec.Replicas {
		t.Fatal("aborted update changed the row")
	}
}

func TestUpdateDatabaseOptionsKeepsConcurrentChange(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := seedProject(t, st)
	db, err := st.CreateDatabase(ctx, &Database{ProjectID: p.ID, Name: "db", Engine: "postgres", Mode: "dedicated", DBName: "d", Username: "u", SizePreset: "micro"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	got, err := st.UpdateDatabaseOptions(ctx, db.ID, func(o *DBOptions) error {
		calls++
		if calls == 1 { // an admin sets the quota while the monitor decides on read-only
			if _, err := st.UpdateDatabaseOptions(ctx, db.ID, func(q *DBOptions) error { q.QuotaMB = 500; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		o.ReadOnly = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.DatabaseByID(ctx, db.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !row.Options.ReadOnly || row.Options.QuotaMB != 500 || !got.ReadOnly || got.QuotaMB != 500 {
		t.Fatalf("calls=%d options=%+v returned=%+v", calls, row.Options, got)
	}
	if row.OptionsVersion != db.OptionsVersion+2 {
		t.Fatalf("options_version = %d, want %d", row.OptionsVersion, db.OptionsVersion+2)
	}
	calls = 0
	_, err = st.UpdateDatabaseOptions(ctx, db.ID, func(o *DBOptions) error {
		calls++
		_, err := st.DB.Exec(ctx, `UPDATE databases SET options_version=options_version+1 WHERE id=$1`, db.ID)
		return err
	})
	if !errors.Is(err, ErrConflict) || calls != casAttempts {
		t.Fatalf("err = %v after %d calls, want ErrConflict", err, calls)
	}
}
