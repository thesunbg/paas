package templates

import (
	"strings"
	"testing"
)

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
      - DB_HOST=db:5432
    command: ["node", "server.js"]
  db:
    image: postgres:16
    volumes: ["pgdata:/var/lib/postgresql/data", "./init:/docker-entrypoint-initdb.d"]
  builder:
    build: .
`)
	plan, err := ParseCompose(yml, ComposeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 2 || len(plan.Databases) != 1 || len(plan.Warnings) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	byName := map[string]Service{}
	for _, s := range plan.Services {
		byName[s.Name] = s
	}
	if w := byName["web"]; w.Port != 80 || w.Type != "web" || w.Private || w.Env["API_URL"] != "http://api:3000" {
		t.Fatalf("web: %+v", w)
	}
	a := byName["api"]
	if a.Port != 3000 || !a.Private || a.Links["DATABASE_URL"] != "db" || a.Env["DB_HOST"] != "${db.db.host}:${db.db.port}" || len(a.Command) != 2 {
		t.Fatalf("api: %+v", a)
	}
	if _, set := a.Env["DATABASE_URL"]; set {
		t.Fatal("linked variable must not be set explicitly")
	}
	if d := plan.Databases[0]; d.Name != "db" || d.Engine != "postgres" || d.Version != "16" {
		t.Fatalf("db: %+v", d)
	}
}

// A monorepo whose compose file lives in deploy/ and builds from the root.
func TestParseComposeRepo(t *testing.T) {
	yml := []byte(`
version: '2.4'
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: family_hub
      POSTGRES_USER: family_hub
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD:?required}
    volumes:
      - pgdata:/var/lib/postgresql/data
      - ./backup:/backup
  server:
    image: ghcr.io/acme/app-server:${TAG:-latest}
    build:
      context: ..
      dockerfile: deploy/Dockerfile.server
    depends_on:
      db: { condition: service_healthy }
    environment:
      DATABASE_URL: postgresql://family_hub:${POSTGRES_PASSWORD}@db:5432/family_hub?schema=public
      PORT: 3001
      SESSION_SECRET: ${SESSION_SECRET:?required}
      WEB_ORIGIN: ${WEB_ORIGIN:?required}
      TELEGRAM_BOT_TOKEN: ${TELEGRAM_BOT_TOKEN:-}
      DB_USER: family_hub
  web:
    build:
      context: ..
      dockerfile: deploy/Dockerfile.web
    depends_on: [server]
    ports: ['127.0.0.1:${APP_PORT:-5599}:80']
`)
	plan, err := ParseCompose(yml, ComposeOptions{Dir: "deploy", Repo: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Services) != 2 || plan.Services[0].Name != "web" || len(plan.Databases) != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	web, srv := plan.Services[0], plan.Services[1]
	if web.Private || web.Port != 80 || web.Build == nil || web.Build.RootDir != "" || web.Build.Dockerfile != "deploy/Dockerfile.web" {
		t.Fatalf("web: %+v %+v", web, web.Build)
	}
	if !srv.Private || srv.Port != 3001 || srv.Build.Dockerfile != "deploy/Dockerfile.server" || srv.Image != "" {
		t.Fatalf("server: %+v", srv)
	}
	if srv.Links["DATABASE_URL"] != "db" || srv.Env["WEB_ORIGIN"] != "${svc.web.url}" || len(srv.Env["SESSION_SECRET"]) != 48 || srv.Env["DB_USER"] != "${db.db.user}" {
		t.Fatalf("server env: %+v links %+v", srv.Env, srv.Links)
	}
	if _, set := srv.Env["TELEGRAM_BOT_TOKEN"]; set {
		t.Fatal("empty optional variables are left unset")
	}
	if len(plan.Warnings) != 0 {
		t.Fatalf("warnings: %v", plan.Warnings)
	}
	got := ExpandWith(srv.Env["WEB_ORIGIN"], nil, map[string]string{"web": "https://web-x.example.com"}, "", "")
	if got != "https://web-x.example.com" {
		t.Fatal(got)
	}
}

func TestComposeBuildOutsideRepo(t *testing.T) {
	_, err := ParseCompose([]byte("services:\n  app:\n    build: ../..\n    ports: [\"80\"]\n"), ComposeOptions{Dir: "deploy", Repo: true})
	if err == nil {
		t.Fatal("a context outside the repository must be rejected")
	}
}

// env_file pointing at an uncommitted .env: the .env.example next to it fills
// in, its localhost MongoDB becomes a managed database, app secrets are
// generated and third-party credentials are reported.
func TestParseComposeEnvFile(t *testing.T) {
	yml := []byte(`
services:
  backend:
    build: { context: ., dockerfile: Dockerfile.backend }
    env_file: .env
    environment:
      - NODE_ENV=production
      - SEED_ON_START=${SEED_ON_START:-courses,exercises}
    ports: ["8888:3000"]
    volumes: [audio-cache:/app/audio-cache]
  frontend:
    build: { context: ., dockerfile: Dockerfile.frontend }
    ports: ["5566:80"]
    depends_on: [backend]
volumes:
  audio-cache:
`)
	example := `# MongoDB (external)
MONGODB_URI=mongodb://localhost:27017/bobi-learning
PORT=3000
NODE_ENV=development
FRONTEND_URL=http://localhost:5173
OPENAI_API_KEY=sk-your-api-key-here
GEMINI_API_KEY=
DEFAULT_AI_PROVIDER=openai
JWT_SECRET=change-this-in-production
JWT_REFRESH_SECRET=change-this-refresh-secret
GOOGLE_CLIENT_ID=
GOOGLE_CALLBACK_URL=http://localhost:3000/api/v1/auth/google/callback
SMTP_FROM=Bobi Learning <noreply@bobi.vn>
SEED_FAIL_FAST=0   # 1 = stop on failure
`
	plan, err := ParseCompose(yml, ComposeOptions{Repo: true, Dir: "", Files: map[string]string{".env.example": example}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Databases) != 1 || plan.Databases[0].Engine != "mongo" {
		t.Fatalf("databases: %+v", plan.Databases)
	}
	var be, fe Service
	for _, s := range plan.Services {
		if s.Name == "backend" {
			be = s
		} else {
			fe = s
		}
	}
	if plan.Services[0].Name != "frontend" {
		t.Fatalf("the entry point (nothing depends on it) comes first: %v", plan.Services[0].Name)
	}
	if be.Links["MONGODB_URI"] != plan.Databases[0].Name {
		t.Fatalf("links: %+v", be.Links)
	}
	if be.Env["NODE_ENV"] != "production" || be.Env["PORT"] != "3000" || be.Env["DEFAULT_AI_PROVIDER"] != "openai" || be.Env["SEED_FAIL_FAST"] != "0" {
		t.Fatalf("env: %+v", be.Env)
	}
	if len(be.Env["JWT_SECRET"]) != 48 || len(be.Env["JWT_REFRESH_SECRET"]) != 48 || be.Env["JWT_SECRET"] == be.Env["JWT_REFRESH_SECRET"] {
		t.Fatalf("secrets: %q %q", be.Env["JWT_SECRET"], be.Env["JWT_REFRESH_SECRET"])
	}
	if be.Env["GOOGLE_CALLBACK_URL"] != "${svc.frontend.url}/api/v1/auth/google/callback" || be.Env["FRONTEND_URL"] != "${svc.frontend.url}" {
		t.Fatalf("urls: %q %q", be.Env["GOOGLE_CALLBACK_URL"], be.Env["FRONTEND_URL"])
	}
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_CLIENT_ID"} {
		if _, set := be.Env[k]; set {
			t.Fatalf("%s must be left to the user", k)
		}
	}
	if be.Env["SMTP_FROM"] != "Bobi Learning <noreply@bobi.vn>" {
		// "<...>" reads as a placeholder; acceptable either way, but not mangled.
		if _, set := be.Env["SMTP_FROM"]; set {
			t.Fatalf("SMTP_FROM: %q", be.Env["SMTP_FROM"])
		}
	}
	if len(fe.DependsOn) != 1 || fe.DependsOn[0] != "backend" {
		t.Fatalf("depends_on: %v", fe.DependsOn)
	}
	found := false
	for _, w := range plan.Warnings {
		if strings.Contains(w, "OPENAI_API_KEY") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings: %v", plan.Warnings)
	}
}

func TestParseDotenv(t *testing.T) {
	got := ParseDotenv("export A=1\nB=\"x # y\"\nC=z # comment\n# D=no\nE='q'\nbad line\n")
	want := [][2]string{{"A", "1"}, {"B", "x # y"}, {"C", "z"}, {"E", "q"}}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
}
