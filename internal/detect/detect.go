// Package detect inspects a source tree and produces a build plan: the
// language/framework and a generated Dockerfile (unless the repo has one).
package detect

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/thesunbg/paas/internal/model"
	"github.com/thesunbg/paas/internal/proto"
)

// DefaultPort is the port every generated image listens on ($PORT).
const DefaultPort = 8080

// Plan is the result of inspecting a repository.
type Plan struct {
	Language       string // node | go | python | php | ruby | rust | java | deno | static | dockerfile
	Framework      string
	Static         bool   // build output is a static site served by the proxy
	Dockerfile     string // generated Dockerfile content; empty when UserDockerfile is set
	UserDockerfile string // path (relative to root) of the repository's own Dockerfile
	Port           int
	Prisma         *proto.Prisma // the app uses Prisma (database the platform can provide)
}

// Summary is a short human-readable description, e.g. "node/next".
func (p *Plan) Summary() string {
	s := p.Language
	if p.Framework != "" {
		s += "/" + p.Framework
	}
	if p.Static {
		s += " (static)"
	}
	return s
}

// Options influence detection.
type Options struct {
	ServiceType  string // web | worker | static
	Build        model.BuildConfig
	Dockerfile   string // explicit Dockerfile path
	BuildEnvKeys []string
}

// Detect inspects dir and returns a plan.
func Detect(dir string, opt Options) (*Plan, error) {
	if opt.Build.Builder != "auto" {
		df := opt.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		if exists(filepath.Join(dir, df)) {
			p := &Plan{Language: "dockerfile", UserDockerfile: df, Port: exposedPort(filepath.Join(dir, df))}
			if opt.ServiceType == model.TypeStatic {
				p.Static = true
			}
			return p, nil
		}
		if opt.Build.Builder == "dockerfile" {
			return nil, fmt.Errorf("dockerfile %s not found", df)
		}
	}
	d := &detector{dir: dir, opt: opt}
	switch {
	case exists(filepath.Join(dir, "package.json")) && !exists(filepath.Join(dir, "deno.json")):
		return d.node()
	case exists(filepath.Join(dir, "go.mod")):
		return d.golang()
	case anyExists(dir, "requirements.txt", "pyproject.toml", "Pipfile", "manage.py"):
		return d.python()
	case anyExists(dir, "composer.json", "index.php"):
		return d.php()
	case exists(filepath.Join(dir, "Cargo.toml")):
		return d.rust()
	case anyExists(dir, "pom.xml", "build.gradle", "build.gradle.kts"):
		return d.java()
	case exists(filepath.Join(dir, "Gemfile")):
		return d.ruby()
	case anyExists(dir, "deno.json", "deno.jsonc"):
		return d.deno()
	case exists(filepath.Join(dir, "index.html")):
		return d.plainStatic()
	}
	return nil, fmt.Errorf("could not detect the language of this repository; add a Dockerfile or set a build command")
}

type detector struct {
	dir string
	opt Options
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func anyExists(dir string, names ...string) bool {
	for _, n := range names {
		if exists(filepath.Join(dir, n)) {
			return true
		}
	}
	return false
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

var exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)

func exposedPort(path string) int {
	m := exposeRe.FindStringSubmatch(readFile(path))
	if len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// secretEnv sources build-time environment from a BuildKit secret so values never land in image layers.
const secretEnv = `if [ -f /run/secrets/paasenv ]; then set -a; . /run/secrets/paasenv; set +a; fi;`

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

func runWithEnv(cmd string) string {
	return "RUN --mount=type=secret,id=paasenv sh -c " + shQuote(secretEnv+" "+cmd)
}

// ---------- Node.js ----------

type packageJSON struct {
	Prisma struct {
		Schema string `json:"schema"`
	} `json:"prisma"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Engines         map[string]string `json:"engines"`
	Main            string            `json:"main"`
	PackageManager  string            `json:"packageManager"`
}

func (p *packageJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

var digitsRe = regexp.MustCompile(`\d+`)

func nodeMajor(pkg *packageJSON, dir string) string {
	candidates := []string{strings.TrimSpace(readFile(filepath.Join(dir, ".nvmrc"))), strings.TrimSpace(readFile(filepath.Join(dir, ".node-version"))), pkg.Engines["node"]}
	for _, c := range candidates {
		if m := digitsRe.FindString(c); m != "" {
			if n, _ := strconv.Atoi(m); n >= 16 && n <= 30 {
				return m
			}
		}
	}
	return "22"
}

func (d *detector) node() (*Plan, error) {
	var pkg packageJSON
	if err := json.Unmarshal([]byte(readFile(filepath.Join(d.dir, "package.json"))), &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	plan := &Plan{Language: "node", Port: DefaultPort}
	switch {
	case pkg.has("next"):
		plan.Framework = "next"
	case pkg.has("nuxt"):
		plan.Framework = "nuxt"
	case pkg.has("@sveltejs/kit"):
		plan.Framework = "sveltekit"
	case pkg.has("@remix-run/node") || pkg.has("@remix-run/serve"):
		plan.Framework = "remix"
	case pkg.has("astro"):
		plan.Framework = "astro"
	case pkg.has("@angular/core"):
		plan.Framework = "angular"
	case pkg.has("react-scripts"):
		plan.Framework = "create-react-app"
	case pkg.has("vite"):
		plan.Framework = "vite"
	case pkg.has("@nestjs/core"):
		plan.Framework = "nestjs"
	case pkg.has("express"):
		plan.Framework = "express"
	case pkg.has("fastify"):
		plan.Framework = "fastify"
	}

	b := d.opt.Build
	_, hasBuild := pkg.Scripts["build"]
	_, hasStart := pkg.Scripts["start"]
	// "main" is often npm-init boilerplate ("index.js") that does not exist.
	mainEntry := ""
	if pkg.Main != "" && exists(filepath.Join(d.dir, pkg.Main)) {
		mainEntry = pkg.Main
	}
	hasServerEntry := mainEntry != "" || anyExists(d.dir, "server.js", "index.js", "app.js", "main.js", "server.mjs", "index.mjs")

	switch {
	case d.opt.ServiceType == model.TypeStatic:
		plan.Static = true
	case plan.Framework == "next" && b.StartCmd == "" && d.nextStaticExport():
		// next.config with output: 'export' builds a static site into out/.
		plan.Static = true
	case d.opt.ServiceType == model.TypeWeb && b.StartCmd == "" && !hasStart && !hasServerEntry:
		// SPA without a server (Vite/CRA/Angular/Astro): serve it as a static site.
		switch plan.Framework {
		case "vite", "create-react-app", "angular", "astro", "":
			if hasBuild {
				plan.Static = true
			}
		}
	}

	pm := "npm"
	image := "node:" + nodeMajor(&pkg, d.dir) + "-alpine"
	switch {
	case anyExists(d.dir, "bun.lockb", "bun.lock") || strings.HasPrefix(pkg.PackageManager, "bun"):
		pm = "bun"
		image = "oven/bun:1-alpine"
	case exists(filepath.Join(d.dir, "pnpm-lock.yaml")) || strings.HasPrefix(pkg.PackageManager, "pnpm"):
		pm = "pnpm"
	case exists(filepath.Join(d.dir, "yarn.lock")) || strings.HasPrefix(pkg.PackageManager, "yarn"):
		pm = "yarn"
	}

	install := b.InstallCmd
	if install == "" {
		switch pm {
		case "bun":
			install = "bun install"
		case "pnpm":
			install = "corepack enable && pnpm install --frozen-lockfile || pnpm install"
		case "yarn":
			install = "corepack enable && (yarn install --frozen-lockfile || yarn install)"
		default:
			if exists(filepath.Join(d.dir, "package-lock.json")) {
				install = "npm ci --no-audit --no-fund"
			} else {
				install = "npm install --no-audit --no-fund"
			}
		}
	}
	build := b.BuildCmd
	if build == "" && hasBuild {
		build = pm + " run build"
	}
	start := b.StartCmd
	if start == "" {
		switch {
		case hasStart:
			start = pm + " run start"
		case mainEntry != "":
			start = "node " + mainEntry
		case plan.Framework == "next":
			start = "npx next start -H 0.0.0.0 -p ${PORT:-8080}"
		case plan.Framework == "nuxt" && hasBuild:
			start = "node .output/server/index.mjs"
		default:
			for _, f := range []string{"server.js", "index.js", "app.js", "main.js", "server.mjs", "index.mjs"} {
				if exists(filepath.Join(d.dir, f)) {
					start = "node " + f
					break
				}
			}
		}
	}
	if !plan.Static && start == "" && d.opt.ServiceType != model.TypeStatic {
		return nil, fmt.Errorf("no start command: add a \"start\" script to package.json or set a start command")
	}

	cache := "/root/.npm"
	switch pm {
	case "pnpm":
		cache = "/root/.local/share/pnpm/store"
	case "yarn":
		cache = "/usr/local/share/.cache/yarn"
	case "bun":
		cache = "/root/.bun/install/cache"
	}

	// Prisma generates its client from the schema, which is not there yet when
	// dependencies install (only package files are copied first, for caching).
	prisma := pkg.has("prisma") || pkg.has("@prisma/client") || exists(filepath.Join(d.dir, "prisma", "schema.prisma"))
	if prisma {
		plan.Prisma = prismaInfo(d.dir, pkg.Prisma.Schema)
		if plan.Prisma != nil && start != "" && b.StartCmd == "" {
			start = prismaStart(plan.Prisma) + start
		}
	}

	var df strings.Builder
	fmt.Fprintf(&df, "# syntax=docker/dockerfile:1\nFROM %s AS build\nWORKDIR /app\n", image)
	if prisma && strings.Contains(image, "alpine") {
		df.WriteString("RUN apk add --no-cache openssl\n") // Prisma's query engine needs libssl
	}
	df.WriteString("COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* bun.lock* .npmrc* ./\n")
	fmt.Fprintf(&df, "RUN --mount=type=cache,target=%s --mount=type=secret,id=paasenv sh -c %s\n", cache, shQuote(secretEnv+" "+install))
	df.WriteString("COPY . .\n")
	if prisma {
		df.WriteString(runWithEnv("npx prisma generate") + "\n")
	}
	if build != "" {
		if plan.Framework == "next" {
			df.WriteString("ENV NEXT_TELEMETRY_DISABLED=1\n")
		}
		df.WriteString(runWithEnv(build) + "\n")
	}
	if plan.Static {
		candidates := []string{"dist", "build", "out", ".output/public", "dist/browser", "_site", "public"}
		if plan.Framework == "next" {
			candidates = []string{"out"}
		}
		writeStaticExport(&df, b.OutputDir, candidates)
		return plan.withDockerfile(df.String()), nil
	}
	fmt.Fprintf(&df, "ENV NODE_ENV=production PORT=%d HOST=0.0.0.0 HOSTNAME=0.0.0.0\nEXPOSE %d\n", DefaultPort, DefaultPort)
	fmt.Fprintf(&df, "CMD [\"sh\", \"-c\", %s]\n", strconv.Quote(start))
	return plan.withDockerfile(df.String()), nil
}

var (
	prismaProviderRe = regexp.MustCompile(`(?s)datasource\s+\w+\s*\{.*?provider\s*=\s*"([a-z]+)"`)
	prismaURLEnvRe   = regexp.MustCompile(`(?s)datasource\s+\w+\s*\{.*?url\s*=\s*env\(\s*"([A-Za-z_][A-Za-z0-9_]*)"\s*\)`)
)

// prismaStart prepares the database before the app starts: a new SQLite file
// starts from the committed one, migrations are applied, then db push adds
// whatever the migrations miss (schemas edited with db push only). Failures are
// reported but do not stop the app, which shows its own error.
func prismaStart(p *proto.Prisma) string {
	var b strings.Builder
	env := firstNonEmptyStr(p.URLEnv, "DATABASE_URL")
	if p.SeedFile != "" {
		fmt.Fprintf(&b, `f="${%[1]s#file:}"; case "$%[1]s" in file:/*) if [ ! -s "$f" ]; then mkdir -p "$(dirname "$f")" && cp %[2]s "$f" && echo "prisma: initial data from %[2]s"; fi;; esac; `, env, shQuote(p.SeedFile))
	}
	if p.Migrations {
		b.WriteString(`npx prisma migrate deploy || echo "prisma migrate deploy failed"; `)
	}
	b.WriteString(`npx prisma db push --skip-generate || echo "prisma db push failed"; `)
	return b.String()
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// prismaInfo reads the datasource of the Prisma schema (prisma/schema.prisma,
// the package.json "prisma.schema" path, or a prisma/schema/ folder).
func prismaInfo(dir, configured string) *proto.Prisma {
	var paths []string
	if configured != "" && !filepath.IsAbs(configured) && !strings.Contains(configured, "..") {
		paths = append(paths, filepath.Join(dir, configured))
	}
	paths = append(paths, filepath.Join(dir, "prisma", "schema.prisma"), filepath.Join(dir, "schema.prisma"))
	if files, _ := filepath.Glob(filepath.Join(dir, "prisma", "schema", "*.prisma")); len(files) > 0 {
		paths = append(paths, files...)
	}
	for _, p := range paths {
		src := readFile(p)
		if !strings.Contains(src, "datasource") {
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				files, _ := filepath.Glob(filepath.Join(p, "*.prisma"))
				for _, f := range files {
					if c := readFile(f); strings.Contains(c, "datasource") {
						src = c
						p = f
						break
					}
				}
			}
		}
		m := prismaProviderRe.FindStringSubmatch(src)
		if m == nil {
			continue
		}
		info := &proto.Prisma{Provider: m[1]}
		if u := prismaURLEnvRe.FindStringSubmatch(src); u != nil {
			info.URLEnv = u[1]
		}
		info.Migrations = exists(filepath.Join(filepath.Dir(p), "migrations"))
		if info.Provider == "sqlite" {
			// A database file committed next to the schema is the app's initial data.
			for _, name := range []string{"dev.db", "data.db", "db.sqlite", "database.db", "prod.db"} {
				if f := filepath.Join(filepath.Dir(p), name); exists(f) {
					info.SeedFile, _ = filepath.Rel(dir, f)
					break
				}
			}
		}
		return info
	}
	return nil
}

var nextExportRe = regexp.MustCompile(`output\s*:\s*["'\x60]export["'\x60]`)

// nextStaticExport reports whether next.config sets output: 'export'.
func (d *detector) nextStaticExport() bool {
	for _, f := range []string{"next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs", "next.config.mts"} {
		if nextExportRe.MatchString(readFile(filepath.Join(d.dir, f))) {
			return true
		}
	}
	return false
}

func (p *Plan) withDockerfile(s string) *Plan {
	p.Dockerfile = s
	if p.Static {
		p.Port = 0
	}
	return p
}

// writeStaticExport copies the first directory containing index.html into a scratch image.
func writeStaticExport(df *strings.Builder, outputDir string, candidates []string) {
	list := strings.Join(candidates, " ")
	if outputDir != "" {
		list = outputDir
	}
	script := fmt.Sprintf(`set -e; OUT=""; for d in %s; do if [ -f "$d/index.html" ]; then OUT="$d"; break; fi; done; `+
		`if [ -z "$OUT" ]; then echo "could not find build output (looked for index.html in: %s); set the output directory" >&2; exit 1; fi; `+
		`echo "static output: $OUT"; mkdir -p /paas-out && cp -a "$OUT"/. /paas-out/`, list, list)
	df.WriteString("RUN sh -c " + shQuote(script) + "\n")
	df.WriteString("FROM scratch\nCOPY --from=build /paas-out/ /\n")
}

func (d *detector) plainStatic() (*Plan, error) {
	plan := &Plan{Language: "static", Static: true}
	df := "# syntax=docker/dockerfile:1\nFROM scratch\nCOPY . /\n"
	if d.opt.Build.BuildCmd != "" {
		var b strings.Builder
		b.WriteString("# syntax=docker/dockerfile:1\nFROM node:22-alpine AS build\nWORKDIR /app\nCOPY . .\n")
		b.WriteString(runWithEnv(d.opt.Build.BuildCmd) + "\n")
		writeStaticExport(&b, d.opt.Build.OutputDir, []string{"dist", "build", "out", "public", "."})
		df = b.String()
	}
	return plan.withDockerfile(df), nil
}

// ---------- Go ----------

var goVersionRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)

func (d *detector) golang() (*Plan, error) {
	plan := &Plan{Language: "go", Port: DefaultPort}
	ver := "1.24"
	if m := goVersionRe.FindStringSubmatch(readFile(filepath.Join(d.dir, "go.mod"))); len(m) == 2 {
		ver = m[1]
	}
	mod := readFile(filepath.Join(d.dir, "go.mod"))
	switch {
	case strings.Contains(mod, "github.com/gin-gonic/gin"):
		plan.Framework = "gin"
	case strings.Contains(mod, "github.com/gofiber/fiber"):
		plan.Framework = "fiber"
	case strings.Contains(mod, "github.com/labstack/echo"):
		plan.Framework = "echo"
	}
	pkg := d.goMainPackage()
	build := d.opt.Build.BuildCmd
	if build == "" {
		build = fmt.Sprintf(`CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /paas-bin/app %s`, pkg)
	}
	start := d.opt.Build.StartCmd
	cmd := `CMD ["/app/server"]`
	if start != "" {
		cmd = fmt.Sprintf(`CMD ["sh", "-c", %s]`, strconv.Quote(start))
	}
	df := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM golang:%s-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build --mount=type=secret,id=paasenv sh -c %s
FROM alpine:3.20
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /usr/local/share/zoneinfo.zip
ENV ZONEINFO=/usr/local/share/zoneinfo.zip
WORKDIR /app
COPY --from=build /paas-bin/app /app/server
COPY --from=build /src /src-assets
ENV PORT=%d
EXPOSE %d
%s
`, ver, shQuote(secretEnv+" mkdir -p /paas-bin && "+build), DefaultPort, DefaultPort, cmd)
	// Keep only runtime assets that commonly sit next to the binary (templates, static files).
	df = strings.Replace(df, "COPY --from=build /src /src-assets\n", d.goAssetCopies(), 1)
	return plan.withDockerfile(df), nil
}

func (d *detector) goAssetCopies() string {
	var b strings.Builder
	for _, dir := range []string{"templates", "static", "public", "views", "web", "assets", "migrations"} {
		if st, err := os.Stat(filepath.Join(d.dir, dir)); err == nil && st.IsDir() {
			fmt.Fprintf(&b, "COPY --from=build /src/%s /app/%s\n", dir, dir)
		}
	}
	return b.String()
}

func (d *detector) goMainPackage() string {
	if hasMainPackage(d.dir) {
		return "."
	}
	entries, _ := os.ReadDir(filepath.Join(d.dir, "cmd"))
	var names []string
	for _, e := range entries {
		if e.IsDir() && hasMainPackage(filepath.Join(d.dir, "cmd", e.Name())) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, pref := range []string{"server", "api", "web", "app"} {
		for _, n := range names {
			if n == pref {
				return "./cmd/" + n
			}
		}
	}
	if len(names) > 0 {
		return "./cmd/" + names[0]
	}
	return "."
}

func hasMainPackage(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(file)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "package ") {
				file.Close()
				if line == "package main" {
					return true
				}
				break
			}
		}
		file.Close()
	}
	return false
}

// ---------- Python ----------

var pyVersionRe = regexp.MustCompile(`3\.\d+`)

func (d *detector) python() (*Plan, error) {
	plan := &Plan{Language: "python", Port: DefaultPort}
	reqs := strings.ToLower(readFile(filepath.Join(d.dir, "requirements.txt")) + readFile(filepath.Join(d.dir, "pyproject.toml")) + readFile(filepath.Join(d.dir, "Pipfile")))
	ver := "3.12"
	for _, f := range []string{".python-version", "runtime.txt"} {
		if m := pyVersionRe.FindString(readFile(filepath.Join(d.dir, f))); m != "" {
			ver = m
			break
		}
	}
	install := d.opt.Build.InstallCmd
	copyDeps := "COPY . .\n"
	if install == "" {
		switch {
		case exists(filepath.Join(d.dir, "requirements.txt")):
			copyDeps = "COPY requirements.txt ./\n"
			install = "pip install -r requirements.txt"
		case exists(filepath.Join(d.dir, "Pipfile")):
			install = "pip install pipenv && pipenv install --system --deploy --ignore-pipfile || pipenv install --system"
		case exists(filepath.Join(d.dir, "pyproject.toml")):
			install = "pip install ."
		default:
			install = "true"
		}
	}
	start := d.opt.Build.StartCmd
	extra := ""
	if start == "" {
		start = procfileWeb(d.dir)
	}
	port := fmt.Sprint(DefaultPort)
	if start == "" {
		switch {
		case exists(filepath.Join(d.dir, "manage.py")):
			plan.Framework = "django"
			if proj := d.djangoProject(); proj != "" {
				start = "python manage.py migrate --noinput; gunicorn " + proj + ".wsgi --bind 0.0.0.0:$PORT --workers ${WEB_CONCURRENCY:-2}"
				if !strings.Contains(reqs, "gunicorn") {
					extra = "gunicorn"
				}
			} else {
				start = "python manage.py runserver 0.0.0.0:$PORT"
			}
		case strings.Contains(reqs, "fastapi"):
			plan.Framework = "fastapi"
			mod := d.findPyApp("FastAPI(")
			start = "uvicorn " + mod + " --host 0.0.0.0 --port $PORT --workers ${WEB_CONCURRENCY:-1}"
			if !strings.Contains(reqs, "uvicorn") {
				extra = "uvicorn"
			}
		case strings.Contains(reqs, "flask"):
			plan.Framework = "flask"
			mod := d.findPyApp("Flask(")
			start = "gunicorn " + mod + " --bind 0.0.0.0:$PORT --workers ${WEB_CONCURRENCY:-2}"
			if !strings.Contains(reqs, "gunicorn") {
				extra = "gunicorn"
			}
		default:
			for _, f := range []string{"main.py", "app.py", "server.py", "bot.py", "worker.py"} {
				if exists(filepath.Join(d.dir, f)) {
					start = "python " + f
					break
				}
			}
		}
	}
	if start == "" {
		return nil, fmt.Errorf("no start command found: add a Procfile, main.py/app.py, or set a start command")
	}
	if extra != "" {
		install += " && pip install " + extra
	}
	var df strings.Builder
	fmt.Fprintf(&df, "# syntax=docker/dockerfile:1\nFROM python:%s-slim\n", ver)
	df.WriteString("ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PIP_DISABLE_PIP_VERSION_CHECK=1\nWORKDIR /app\n")
	df.WriteString(copyDeps)
	fmt.Fprintf(&df, "RUN --mount=type=cache,target=/root/.cache/pip --mount=type=secret,id=paasenv sh -c %s\n", shQuote(secretEnv+" "+install))
	df.WriteString("COPY . .\n")
	if d.opt.Build.BuildCmd != "" {
		df.WriteString(runWithEnv(d.opt.Build.BuildCmd) + "\n")
	} else if plan.Framework == "django" {
		df.WriteString("RUN python manage.py collectstatic --noinput >/dev/null 2>&1 || true\n")
	}
	fmt.Fprintf(&df, "ENV PORT=%s\nEXPOSE %s\nCMD [\"sh\", \"-c\", %s]\n", port, port, strconv.Quote(start))
	return plan.withDockerfile(df.String()), nil
}

func procfileWeb(dir string) string {
	for _, line := range strings.Split(readFile(filepath.Join(dir, "Procfile")), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "web:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "web:"))
		}
	}
	return ""
}

func (d *detector) djangoProject() string {
	matches, _ := filepath.Glob(filepath.Join(d.dir, "*", "wsgi.py"))
	if len(matches) > 0 {
		return filepath.Base(filepath.Dir(matches[0]))
	}
	return ""
}

// findPyApp returns "module:variable" for the file that constructs the app.
func (d *detector) findPyApp(marker string) string {
	re := regexp.MustCompile(`(?m)^(\w+)\s*=\s*` + regexp.QuoteMeta(marker))
	for _, f := range []string{"main.py", "app.py", "server.py", "api.py", "app/main.py", "src/main.py"} {
		if m := re.FindStringSubmatch(readFile(filepath.Join(d.dir, f))); len(m) == 2 {
			mod := strings.TrimSuffix(strings.ReplaceAll(f, "/", "."), ".py")
			return mod + ":" + m[1]
		}
	}
	return "main:app"
}

// ---------- PHP ----------

func (d *detector) php() (*Plan, error) {
	plan := &Plan{Language: "php", Port: DefaultPort}
	if exists(filepath.Join(d.dir, "artisan")) {
		plan.Framework = "laravel"
	}
	var df strings.Builder
	df.WriteString("# syntax=docker/dockerfile:1\n")
	hasComposer := exists(filepath.Join(d.dir, "composer.json"))
	if hasComposer {
		df.WriteString("FROM composer:2 AS vendor\nWORKDIR /app\nCOPY composer.json composer.lock* ./\n")
		df.WriteString("RUN --mount=type=cache,target=/tmp/cache composer install --no-dev --no-scripts --no-autoloader --prefer-dist --no-interaction --ignore-platform-reqs\n")
		df.WriteString("COPY . .\nRUN composer dump-autoload --optimize --no-dev --ignore-platform-reqs\n")
	}
	df.WriteString("FROM dunglas/frankenphp:1-php8.3-alpine\n")
	root := "/app"
	if exists(filepath.Join(d.dir, "public")) {
		root = "/app/public"
	}
	fmt.Fprintf(&df, "ENV SERVER_NAME=\":%d\" SERVER_ROOT=%s PORT=%d\nWORKDIR /app\n", DefaultPort, root, DefaultPort)
	if hasComposer {
		df.WriteString("COPY --from=vendor /app /app\n")
	} else {
		df.WriteString("COPY . /app\n")
	}
	if plan.Framework == "laravel" {
		df.WriteString("RUN chown -R www-data:www-data storage bootstrap/cache 2>/dev/null || true\n")
	}
	if d.opt.Build.BuildCmd != "" {
		df.WriteString(runWithEnv(d.opt.Build.BuildCmd) + "\n")
	}
	fmt.Fprintf(&df, "EXPOSE %d\n", DefaultPort)
	if d.opt.Build.StartCmd != "" {
		fmt.Fprintf(&df, "CMD [\"sh\", \"-c\", %s]\n", strconv.Quote(d.opt.Build.StartCmd))
	}
	return plan.withDockerfile(df.String()), nil
}

// ---------- Rust ----------

var cargoNameRe = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)

func (d *detector) rust() (*Plan, error) {
	plan := &Plan{Language: "rust", Port: DefaultPort}
	name := "app"
	if m := cargoNameRe.FindStringSubmatch(readFile(filepath.Join(d.dir, "Cargo.toml"))); len(m) == 2 {
		name = m[1]
	}
	start := `CMD ["/app/server"]`
	if d.opt.Build.StartCmd != "" {
		start = fmt.Sprintf(`CMD ["sh", "-c", %s]`, strconv.Quote(d.opt.Build.StartCmd))
	}
	df := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM rust:1-slim AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=/usr/local/cargo/registry --mount=type=cache,target=/src/target --mount=type=secret,id=paasenv sh -c %s
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /paas-bin/server /app/server
ENV PORT=%d
EXPOSE %d
%s
`, shQuote(secretEnv+" cargo build --release && mkdir -p /paas-bin && cp target/release/"+name+" /paas-bin/server"), DefaultPort, DefaultPort, start)
	return plan.withDockerfile(df), nil
}

// ---------- Java ----------

func (d *detector) java() (*Plan, error) {
	plan := &Plan{Language: "java", Port: DefaultPort}
	var builder, build, jar string
	if exists(filepath.Join(d.dir, "pom.xml")) {
		plan.Framework = "maven"
		builder = "maven:3.9-eclipse-temurin-21"
		build = "mvn -q -DskipTests package && cp $(ls target/*.jar | grep -v original | head -1) /paas-app.jar"
		if exists(filepath.Join(d.dir, "mvnw")) {
			build = "chmod +x mvnw && ./mvnw -q -DskipTests package && cp $(ls target/*.jar | grep -v original | head -1) /paas-app.jar"
		}
		jar = "/root/.m2"
	} else {
		plan.Framework = "gradle"
		builder = "gradle:8-jdk21"
		build = "gradle --no-daemon -q bootJar 2>/dev/null || gradle --no-daemon -q build -x test; cp $(ls build/libs/*.jar | grep -v plain | head -1) /paas-app.jar"
		jar = "/home/gradle/.gradle"
	}
	if d.opt.Build.BuildCmd != "" {
		build = d.opt.Build.BuildCmd
	}
	start := `exec java $JAVA_TOOL_OPTIONS -jar /app/app.jar --server.port=$PORT`
	if d.opt.Build.StartCmd != "" {
		start = d.opt.Build.StartCmd
	}
	df := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM %s AS build
WORKDIR /src
COPY . .
RUN --mount=type=cache,target=%s --mount=type=secret,id=paasenv sh -c %s
FROM eclipse-temurin:21-jre-alpine
WORKDIR /app
COPY --from=build /paas-app.jar /app/app.jar
ENV PORT=%d
EXPOSE %d
CMD ["sh", "-c", %s]
`, builder, jar, shQuote(secretEnv+" "+build), DefaultPort, DefaultPort, strconv.Quote(start))
	return plan.withDockerfile(df), nil
}

// ---------- Ruby ----------

func (d *detector) ruby() (*Plan, error) {
	plan := &Plan{Language: "ruby", Port: DefaultPort}
	gemfile := readFile(filepath.Join(d.dir, "Gemfile"))
	start := d.opt.Build.StartCmd
	if start == "" {
		start = procfileWeb(d.dir)
	}
	if start == "" {
		switch {
		case strings.Contains(gemfile, "rails"):
			plan.Framework = "rails"
			start = "bundle exec rails db:prepare 2>/dev/null; bundle exec rails server -b 0.0.0.0 -p $PORT"
		case exists(filepath.Join(d.dir, "config.ru")):
			start = "bundle exec rackup -o 0.0.0.0 -p $PORT"
		default:
			start = "bundle exec ruby app.rb -o 0.0.0.0 -p $PORT"
		}
	}
	df := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM ruby:3.3-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential libpq-dev libyaml-dev && rm -rf /var/lib/apt/lists/*
ENV RAILS_ENV=production RACK_ENV=production BUNDLE_WITHOUT=development:test RAILS_LOG_TO_STDOUT=1 RAILS_SERVE_STATIC_FILES=1
WORKDIR /app
COPY Gemfile Gemfile.lock* ./
RUN --mount=type=cache,target=/usr/local/bundle/cache bundle install
COPY . .
ENV PORT=%d
EXPOSE %d
CMD ["sh", "-c", %s]
`, DefaultPort, DefaultPort, strconv.Quote(start))
	return plan.withDockerfile(df), nil
}

// ---------- Deno ----------

func (d *detector) deno() (*Plan, error) {
	plan := &Plan{Language: "deno", Port: DefaultPort}
	start := d.opt.Build.StartCmd
	if start == "" {
		cfg := readFile(filepath.Join(d.dir, "deno.json")) + readFile(filepath.Join(d.dir, "deno.jsonc"))
		if strings.Contains(cfg, `"start"`) {
			start = "deno task start"
		} else {
			for _, f := range []string{"main.ts", "server.ts", "mod.ts", "main.js"} {
				if exists(filepath.Join(d.dir, f)) {
					start = "deno run --allow-net --allow-env --allow-read " + f
					break
				}
			}
		}
	}
	if start == "" {
		return nil, fmt.Errorf("no start command: add a \"start\" task to deno.json")
	}
	df := fmt.Sprintf(`# syntax=docker/dockerfile:1
FROM denoland/deno:alpine
WORKDIR /app
COPY . .
RUN deno cache $(ls main.ts server.ts mod.ts 2>/dev/null | head -1) 2>/dev/null || true
ENV PORT=%d
EXPOSE %d
CMD ["sh", "-c", %s]
`, DefaultPort, DefaultPort, strconv.Quote(start))
	return plan.withDockerfile(df), nil
}
