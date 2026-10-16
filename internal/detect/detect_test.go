package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/model"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		svcType  string
		lang     string
		fw       string
		static   bool
		contains []string
	}{
		{
			name:    "next",
			files:   map[string]string{"package.json": `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"15"}}`, "package-lock.json": "{}"},
			svcType: model.TypeWeb,
			lang:    "node", fw: "next",
			contains: []string{"npm ci", "npm run build", "npm run start", "FROM node:22-alpine"},
		},
		{
			name: "next static export with boilerplate main",
			files: map[string]string{
				"package.json":      `{"main":"index.js","scripts":{"build":"next build"},"dependencies":{"next":"15"}}`,
				"package-lock.json": "{}", ".nvmrc": "22",
				"next.config.mjs": "const nextConfig = {\n  output: 'export',\n  trailingSlash: true,\n};\nexport default nextConfig;\n",
			},
			svcType: model.TypeWeb,
			lang:    "node", fw: "next", static: true,
			contains: []string{"npm ci", "npm run build", "FROM scratch", "out"},
		},
		{
			name: "next with prisma",
			files: map[string]string{
				"package.json":         `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"16","@prisma/client":"5"},"devDependencies":{"prisma":"5"}}`,
				"package-lock.json":    "{}",
				"prisma/schema.prisma": "generator client {\n  provider = \"prisma-client-js\"\n}\n",
			},
			svcType: model.TypeWeb,
			lang:    "node", fw: "next",
			contains: []string{"apk add --no-cache openssl", "npx prisma generate", "npm run build"},
		},
		{
			name:    "next without start script",
			files:   map[string]string{"package.json": `{"main":"index.js","scripts":{"build":"next build"},"dependencies":{"next":"15"}}`},
			svcType: model.TypeWeb,
			lang:    "node", fw: "next",
			contains: []string{"npx next start"},
		},
		{
			name:    "vite spa becomes static",
			files:   map[string]string{"package.json": `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"5"}}`, "pnpm-lock.yaml": ""},
			svcType: model.TypeWeb,
			lang:    "node", fw: "vite", static: true,
			contains: []string{"pnpm install", "FROM scratch", "dist"},
		},
		{
			name:    "express with engines",
			files:   map[string]string{"package.json": `{"main":"server.js","engines":{"node":">=20"},"dependencies":{"express":"4"}}`, "server.js": "require(\"express\")"},
			svcType: model.TypeWeb,
			lang:    "node", fw: "express",
			contains: []string{"node:20-alpine", "node server.js", "npm install"},
		},
		{
			name:     "go cmd",
			files:    map[string]string{"go.mod": "module x\n\ngo 1.23\n", "cmd/server/main.go": "package main\nfunc main(){}\n", "templates/a.html": "x"},
			svcType:  model.TypeWeb,
			lang:     "go",
			contains: []string{"golang:1.23-alpine", "./cmd/server", "COPY --from=build /src/templates"},
		},
		{
			name:    "fastapi",
			files:   map[string]string{"requirements.txt": "fastapi\nuvicorn\n", "main.py": "from fastapi import FastAPI\napp = FastAPI()\n"},
			svcType: model.TypeWeb,
			lang:    "python", fw: "fastapi",
			contains: []string{"uvicorn main:app", "pip install -r requirements.txt"},
		},
		{
			name:    "django",
			files:   map[string]string{"requirements.txt": "django\n", "manage.py": "", "mysite/wsgi.py": ""},
			svcType: model.TypeWeb,
			lang:    "python", fw: "django",
			contains: []string{"gunicorn mysite.wsgi", "pip install gunicorn"},
		},
		{
			name:    "plain html",
			files:   map[string]string{"index.html": "<h1>hi</h1>"},
			svcType: model.TypeStatic,
			lang:    "static", static: true,
			contains: []string{"FROM scratch"},
		},
		{
			name:    "laravel",
			files:   map[string]string{"composer.json": "{}", "artisan": "", "public/index.php": ""},
			svcType: model.TypeWeb,
			lang:    "php", fw: "laravel",
			contains: []string{"frankenphp", "SERVER_ROOT=/app/public", "composer install"},
		},
		{
			name:    "dockerfile wins",
			files:   map[string]string{"Dockerfile": "FROM alpine\nEXPOSE 9000\n", "package.json": "{}"},
			svcType: model.TypeWeb,
			lang:    "dockerfile",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, tc.files)
			p, err := Detect(dir, Options{ServiceType: tc.svcType})
			if err != nil {
				t.Fatal(err)
			}
			if p.Language != tc.lang || p.Framework != tc.fw || p.Static != tc.static {
				t.Fatalf("got %s/%s static=%v", p.Language, p.Framework, p.Static)
			}
			for _, c := range tc.contains {
				if !strings.Contains(p.Dockerfile, c) {
					t.Errorf("dockerfile missing %q:\n%s", c, p.Dockerfile)
				}
			}
			if tc.lang == "dockerfile" && p.Port != 9000 {
				t.Errorf("expected EXPOSE port 9000, got %d", p.Port)
			}
		})
	}
}

func TestNoStartCommand(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"package.json": `{"dependencies":{"lodash":"4"}}`})
	if _, err := Detect(dir, Options{ServiceType: model.TypeWorker}); err == nil {
		t.Fatal("expected error for missing start command")
	}
}

func TestPrismaInfo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"package.json":         `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"16","@prisma/client":"5"}}`,
		"prisma/schema.prisma": "generator client {\n  provider = \"prisma-client-js\"\n}\n\ndatasource db {\n  provider = \"sqlite\"\n  url      = env(\"DATABASE_URL\")\n}\n",
		"prisma/migrations/20240101_init/migration.sql": "CREATE TABLE x (id int);",
	})
	p, err := Detect(dir, Options{ServiceType: model.TypeWeb})
	if err != nil {
		t.Fatal(err)
	}
	if p.Prisma == nil || p.Prisma.Provider != "sqlite" || p.Prisma.URLEnv != "DATABASE_URL" || !p.Prisma.Migrations {
		t.Fatalf("prisma info %+v", p.Prisma)
	}
	if !strings.Contains(p.Dockerfile, "npx prisma migrate deploy && npm run start") {
		t.Fatalf("start command does not apply migrations:\n%s", p.Dockerfile)
	}
	// Without migrations the schema is pushed.
	os.RemoveAll(filepath.Join(dir, "prisma", "migrations"))
	p, _ = Detect(dir, Options{ServiceType: model.TypeWeb})
	if !strings.Contains(p.Dockerfile, "npx prisma db push --skip-generate && npm run start") {
		t.Fatalf("expected db push:\n%s", p.Dockerfile)
	}
}
