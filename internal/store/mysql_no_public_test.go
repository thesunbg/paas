package store

import (
	"context"
	"testing"
)

func TestMigrationDropsMySQLPublicPort(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	p := seedProject(t, st)
	mk := func(engine, name string, port int) *Database {
		d, err := st.CreateDatabase(ctx, &Database{ProjectID: p.ID, Name: name, Engine: engine, Mode: "dedicated", DBName: name, Username: name, SizePreset: "nano"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.UpdateDatabaseOptions(ctx, d.ID, func(o *DBOptions) error {
			o.PublicPort, o.AllowedCIDRs = port, []string{"10.0.0.0/8"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		d, err = st.DatabaseByID(ctx, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	my, pg := mk("mysql", "my", 20001), mk("postgres", "pg", 20002)

	if my.Options.PublicPort != 20001 {
		t.Fatalf("seed: mysql has no public port: %+v", my.Options)
	}
	body, err := migrationsFS.ReadFile("migrations/017_mysql_no_public.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(ctx, string(body)); err != nil {
		t.Fatal(err)
	}

	got, err := st.DatabaseByID(ctx, my.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.PublicPort != 0 || len(got.Options.AllowedCIDRs) != 0 {
		t.Errorf("mysql keeps public access: %+v", got.Options)
	}
	if got.OptionsVersion <= my.OptionsVersion {
		t.Errorf("options_version %d not bumped from %d", got.OptionsVersion, my.OptionsVersion)
	}
	got, err = st.DatabaseByID(ctx, pg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Options.PublicPort != 20002 || got.OptionsVersion != pg.OptionsVersion {
		t.Errorf("postgres touched: %+v v%d", got.Options, got.OptionsVersion)
	}
	// 019 renames the engine; the closed port must survive it, for either engine name.
	split, err := migrationsFS.ReadFile("migrations/019_split_mariadb.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(ctx, string(split)); err != nil {
		t.Fatal(err)
	}
	if got, err = st.DatabaseByID(ctx, my.ID); err != nil || got.Engine != "mariadb" || got.Options.PublicPort != 0 {
		t.Errorf("after split: %v engine=%q options=%+v", err, got.Engine, got.Options)
	}
	used, err := st.UsedPublicPorts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if used[20001] || !used[20002] {
		t.Errorf("ports in use: %v", used)
	}
}
