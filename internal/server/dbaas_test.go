package server

import (
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/config"
	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/store"
)

func TestExternalURLAlwaysTLS(t *testing.T) {
	for _, publicURL := range []string{"http://x.test", "https://x.test"} {
		s := &Server{cfg: config.Server{DBHost: "db.test", PublicURL: publicURL}}
		for _, e := range []string{databases.Postgres, databases.Redis, databases.Mongo} {
			db := &store.Database{Engine: e, Options: store.DBOptions{PublicPort: 25432}}
			u := s.externalURL(db, databases.Credentials{Username: "u", Password: "p", DBName: "d"})
			if u == "" || strings.Contains(u, "sslmode=disable") {
				t.Fatalf("%s: %q", e, u)
			}
			want := map[string]string{databases.Postgres: "sslmode=require", databases.Redis: "rediss://", databases.Mongo: "tls=true"}[e]
			if !strings.Contains(u, want) {
				t.Fatalf("%s: %q lacks %s", e, u, want)
			}
		}
	}
}

func TestTCPModePlainEngines(t *testing.T) {
	for _, e := range []string{databases.MySQL, databases.MariaDB} {
		if tcpMode(e) != "plain" {
			t.Fatalf("%s should be plain (never exposed)", e)
		}
	}
}
