package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/store"
	"github.com/thesunbg/paas/internal/templates"
)

// Every version a template names must be creatable; an unset one falls back to the engine default.
func TestTemplateVersionsCreatable(t *testing.T) {
	for _, tpl := range templates.All {
		for _, d := range tpl.Databases {
			v := d.Version
			if v == "" {
				v = databases.Engines[d.Engine].DefaultVersion
			}
			if _, err := databases.CheckCreate(d.Engine, v, time.Now()); err != nil {
				t.Errorf("template %s: %v", tpl.ID, err)
			}
		}
	}
}

func TestCreateDatabaseVersionGate(t *testing.T) {
	s, h := newTestServer(t)
	uid := userWithTeam(t, s, "u@example.com", false)
	tok := mustLogin(t, h, "u@example.com", "password1")
	teams, _ := s.store.TeamsForUser(context.Background(), uid)
	p, err := s.store.CreateProject(context.Background(), teams[0].ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		engine, version string
		want            int
	}{
		{"mariadb", "10.6", 409},
		{"mysql", "10.6", 400},
		{"mongo", "6.0", 409},
		{"postgres", "17@sha256:0123456789abcdef", 400},
		{"postgres", "../x", 400},
	} {
		w := do(t, h, "POST", "/api/v1/projects/"+p.ID+"/databases", tok, map[string]string{"engine": c.engine, "version": c.version, "mode": "dedicated"})
		if w.Code != c.want {
			t.Errorf("%s %s = %d %s, want %d", c.engine, c.version, w.Code, w.Body, c.want)
		}
	}
	w := do(t, h, "GET", "/api/v1/sizes", tok, nil)
	var out struct {
		Engines map[string]struct {
			Versions []databases.VersionInfo `json:"versions"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("sizes = %d %s %v", w.Code, w.Body, err)
	}
	var got []databases.VersionInfo = out.Engines["mongo"].Versions
	if len(got) != 3 || got[2].Name != "6.0" || got[2].Status != "eol" || got[2].EOL != "2025-07-31" {
		t.Fatalf("mongo catalog = %+v", got)
	}
}

func TestEOLTickNotifiesOncePerStatus(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	u := mkUser(t, s, "u@example.com", "password1", false)
	team, err := s.store.CreateTeam(ctx, "t", "t", u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.store.CreateProject(ctx, team.ID, "p", "p")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.store.CreateDatabase(ctx, &store.Database{ProjectID: p.ID, Name: "d", Engine: "postgres", Version: "14", Mode: "dedicated", DBName: "d", Username: "u", SizePreset: "nano"})
	if err != nil {
		t.Fatal(err)
	}
	before := db.OptionsVersion
	pg14 := databases.Engines["postgres"].Versions[4].EOL
	notified := func() string {
		var v string
		_ = s.store.Q(ctx).QueryRow(ctx, `SELECT eol_notified FROM databases WHERE id=$1`, db.ID).Scan(&v)
		return v
	}
	s.eolTick(ctx, pg14.Add(-24*time.Hour))
	if notified() != "ending" {
		t.Fatalf("eol_notified = %q, want ending", notified())
	}
	if fresh, _ := s.store.MarkEOLNotified(ctx, db.ID, "ending"); fresh {
		t.Fatal("same status reported fresh again")
	}
	s.eolTick(ctx, pg14.Add(time.Hour))
	if notified() != "eol" {
		t.Fatalf("eol_notified = %q, want eol", notified())
	}
	if got, _ := s.store.DatabaseByID(ctx, db.ID); got.OptionsVersion != before {
		t.Fatalf("options_version moved %d -> %d", before, got.OptionsVersion)
	}
}
