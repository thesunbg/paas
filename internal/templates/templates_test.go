package templates

import "testing"

func TestExpand(t *testing.T) {
	dbs := map[string]DBValues{"wp": {Host: "shared-mysql.system.internal", Port: "3306", User: "u", Password: "p", Name: "n", URL: "mysql://u:p@h/n"}}
	if got := Expand("${db.wp.host}:${db.wp.port}", dbs, "https://x", "x"); got != "shared-mysql.system.internal:3306" {
		t.Fatal(got)
	}
	if got := Expand("${url}/hook", dbs, "https://app.example.com", ""); got != "https://app.example.com/hook" {
		t.Fatal(got)
	}
	if a, b := Expand("${random}", nil, "", ""), Expand("${random}", nil, "", ""); a == b || len(a) != 48 {
		t.Fatal("random must be unique 48 hex chars")
	}
	if got := Expand("${db.missing.host}", dbs, "", ""); got != "${db.missing.host}" {
		t.Fatal(got)
	}
	for _, tpl := range All {
		for _, s := range tpl.Services {
			for k, v := range s.Env {
				out := Expand(v, map[string]DBValues{"wordpress": {}, "n8n": {}, "umami": {}, "gitea": {}, "metabase": {}}, "u", "h")
				if placeholder.MatchString(out) {
					t.Errorf("%s/%s: unresolved placeholder in %s=%s", tpl.ID, s.Name, k, out)
				}
			}
		}
	}
}

func TestParseCompose(t *testing.T) {
	yml := []byte(`
services:
  web:
    image: nginx:alpine
    ports: ["8080:80"]
    environment:
      API_URL: http://api:3000
  api:
    image: ghcr.io/acme/api:1
    expose: ["3000"]
    environment:
      - DATABASE_URL=postgres://db/app
    command: ["node", "server.js"]
  db:
    image: postgres:16
    volumes: ["pgdata:/var/lib/postgresql/data", "./init:/docker-entrypoint-initdb.d"]
  builder:
    build: .
`)
	svcs, warns, err := ParseCompose(yml)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 3 || len(warns) != 2 {
		t.Fatalf("services=%d warnings=%v", len(svcs), warns)
	}
	byName := map[string]Service{}
	for _, s := range svcs {
		byName[s.Name] = s
	}
	if w := byName["web"]; w.Port != 80 || w.Type != "web" || w.Env["API_URL"] != "http://api:3000" {
		t.Fatalf("web: %+v", w)
	}
	if a := byName["api"]; a.Port != 3000 || a.Env["DATABASE_URL"] != "postgres://db/app" || len(a.Command) != 2 {
		t.Fatalf("api: %+v", a)
	}
	if d := byName["db"]; d.Type != "worker" || len(d.Volumes) != 1 || d.Volumes[0].Name != "pgdata" {
		t.Fatalf("db: %+v", d)
	}
}
