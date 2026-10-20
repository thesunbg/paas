package templates

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/thesunbg/paas/internal/model"
)

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"`
	Ports       []any     `yaml:"ports"`
	Expose      []any     `yaml:"expose"`
	Environment yaml.Node `yaml:"environment"`
	Volumes     []any     `yaml:"volumes"`
	Command     yaml.Node `yaml:"command"`
	DependsOn   yaml.Node `yaml:"depends_on"`
	EnvFile     yaml.Node `yaml:"env_file"`
}

// ComposePlan is what a docker-compose.yml turns into: services (built from the
// repository or run from an image) and managed databases replacing the
// database containers.
type ComposePlan struct {
	Services  []Service  `json:"services"`
	Databases []Database `json:"databases"`
	Warnings  []string   `json:"warnings"`
}

// ComposeOptions say where the compose file comes from.
type ComposeOptions struct {
	// Dir is the directory of the compose file inside the repository ("." for
	// the root); build contexts are resolved against it. Empty means the file
	// was pasted and build contexts cannot be used.
	Dir string
	// Repo is set when services with a build section can be built from a repository.
	Repo bool
	// Files holds repository files the compose file refers to (env files and
	// their .example variants), by path relative to the repository root.
	Files map[string]string
}

// dbImages maps database images to managed engines.
var dbImages = map[string]string{
	"postgres": "postgres", "postgis": "postgres", "timescaledb": "postgres",
	"mysql": "mysql", "mariadb": "mariadb",
	"mongo": "mongo", "mongodb": "mongo", "mongodb-community-server": "mongo",
	"redis": "redis", "valkey": "redis", "redis-stack-server": "redis",
}

// dbEnvRoles maps the configuration variables of database images to the
// connection detail they hold, so other services' copies can be rewritten.
var dbEnvRoles = map[string]string{
	"POSTGRES_USER": "user", "POSTGRES_PASSWORD": "password", "POSTGRES_DB": "name",
	"MYSQL_USER": "user", "MYSQL_PASSWORD": "password", "MYSQL_DATABASE": "name",
	"MARIADB_USER": "user", "MARIADB_PASSWORD": "password", "MARIADB_DATABASE": "name",
	"MONGO_INITDB_ROOT_USERNAME": "user", "MONGO_INITDB_ROOT_PASSWORD": "password", "MONGO_INITDB_DATABASE": "name",
}

// imageEngine returns the managed engine and version for a database image.
func imageEngine(image string) (engine, version string) {
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	tag := ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	engine = dbImages[name]
	if engine == "" {
		return "", ""
	}
	// "16-alpine" -> "16", "11.4.2-jammy" -> "11.4.2"
	version = strings.SplitN(tag, "-", 2)[0]
	if version == "latest" {
		version = ""
	}
	return engine, version
}

// ParseCompose converts a docker-compose.yml into services and managed databases.
// Database containers (postgres, mysql/mariadb, mongo, redis) become managed
// databases; references to them in other services become links or
// ${db.<name>.*} placeholders. Services keep their compose names, so they reach
// each other by those names (DNS search domain of the project).
func ParseCompose(data []byte, opt ComposeOptions) (*ComposePlan, error) {
	var f composeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("invalid compose file: %w", err)
	}
	if len(f.Services) == 0 {
		return nil, fmt.Errorf("compose file has no services")
	}
	names := make([]string, 0, len(f.Services))
	for n := range f.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	plan := &ComposePlan{Services: []Service{}, Databases: []Database{}, Warnings: []string{}}
	warn := func(format string, a ...any) { plan.Warnings = append(plan.Warnings, fmt.Sprintf(format, a...)) }

	ip := newInterpolator()
	// Pass 1: databases, and the public service URL references point to.
	dbs := map[string]bool{}
	primary := ""
	for _, name := range names {
		cs := f.Services[name]
		if isBuild(cs) {
			if primary == "" && len(cs.Ports) > 0 {
				primary = name
			}
			continue
		}
		if engine, version := imageEngine(ip.expand(cs.Image)); engine != "" {
			dbs[name] = true
			plan.Databases = append(plan.Databases, Database{Name: name, Engine: engine, Version: version})
			continue
		}
		if primary == "" && len(cs.Ports) > 0 {
			primary = name
		}
	}
	// The public URL others point at is the entry point: a published service
	// nothing else depends on (the frontend, not the API behind it).
	for _, name := range names {
		cs := f.Services[name]
		if !dbs[name] && len(cs.Ports) > 0 && !isDependency(name, f.Services) {
			primary = name
			break
		}
	}
	ip.primary = primary
	envOf := map[string]map[string]string{}
	fileLinks := map[string]map[string]string{}
	inputs := map[string][]EnvInput{}
	for _, name := range names {
		if dbs[name] {
			envOf[name] = composeEnv(f.Services[name].Environment, ip)
			continue
		}
		env := map[string]string{}
		links := map[string]string{}
		for _, ref := range envFilePaths(f.Services[name].EnvFile) {
			content, from, example := opt.envFile(ref)
			if from == "" {
				warn("%s: env file %s is not in the repository; set its variables in the service's environment", name, ref)
				continue
			}
			fe, fl, in := fileEnv(content, example, primary, plan, dbs)
			for k, v := range fe {
				env[k] = v
			}
			for k, v := range fl {
				links[k] = v
			}
			for _, i := range in {
				i.From = from
				inputs[name] = append(inputs[name], i)
			}
		}
		for k, v := range composeEnv(f.Services[name].Environment, ip) {
			env[k] = v
			delete(links, k)
		}
		envOf[name], fileLinks[name] = env, links
	}

	for _, name := range names {
		if dbs[name] {
			continue
		}
		cs := f.Services[name]
		svc := Service{Name: name, Type: "worker", Size: "small", Env: map[string]string{}}
		if isBuild(cs) {
			if !opt.Repo {
				warn("%s: skipped (it is built from source; import the repository instead)", name)
				continue
			}
			b, err := composeBuild(cs.Build, opt.Dir, ip)
			if err != nil {
				warn("%s: skipped (%v)", name, err)
				continue
			}
			svc.Build = b
		} else {
			svc.Image = ip.expand(cs.Image)
			if svc.Image == "" {
				warn("%s: skipped (no image or build section)", name)
				continue
			}
		}
		for _, p := range cs.Ports {
			if port := containerPort(ip.expand(fmt.Sprint(p))); port > 0 {
				svc.Port, svc.Type = port, "web"
				break
			}
		}
		if svc.Port == 0 {
			svc.Private = true
			for _, e := range cs.Expose {
				if port, err := strconv.Atoi(strings.Split(ip.expand(fmt.Sprint(e)), "/")[0]); err == nil {
					svc.Port, svc.Type = port, "web"
					break
				}
			}
		}
		if svc.Port == 0 {
			if port, err := strconv.Atoi(envOf[name]["PORT"]); err == nil && port > 0 {
				svc.Port, svc.Type = port, "web"
			}
		}
		if svc.Port == 0 && svc.Build != nil && len(cs.Ports) == 0 && len(cs.Expose) == 0 && isDependency(name, f.Services) {
			// Another service depends on it: most likely an internal API. The
			// port comes from the Dockerfile's EXPOSE at build time.
			svc.Port, svc.Type = DefaultBuildPort, "web"
		}
		for _, in := range inputs[name] {
			if _, set := envOf[name][in.Key]; !set {
				svc.Inputs = append(svc.Inputs, in)
			}
		}
		for k, db := range fileLinks[name] {
			if svc.Links == nil {
				svc.Links = map[string]string{}
			}
			svc.Links[k] = db
		}
		for _, d := range dependsOn(cs.DependsOn) {
			if !dbs[d] {
				svc.DependsOn = append(svc.DependsOn, d)
			}
		}
		for k, v := range envOf[name] {
			if db, ok := dbURLRef(v, dbs); ok {
				if svc.Links == nil {
					svc.Links = map[string]string{}
				}
				svc.Links[k] = db
				continue
			}
			if v == "" {
				continue
			}
			svc.Env[k] = dbValueRef(k, v, dbs, envOf)
		}
		for _, raw := range cs.Volumes {
			v, ok := raw.(string)
			if !ok {
				warn("%s: long-form volume ignored", name)
				continue
			}
			parts := strings.Split(ip.expand(v), ":")
			if len(parts) < 2 {
				continue
			}
			src, dst := parts[0], parts[1]
			if strings.HasPrefix(src, ".") || strings.HasPrefix(src, "/") || strings.HasPrefix(src, "~") {
				warn("%s: bind mount %s ignored (use a named volume)", name, v)
				continue
			}
			svc.Volumes = append(svc.Volumes, model.Volume{Name: sanitize(src), Path: dst})
		}
		switch cs.Command.Kind {
		case yaml.ScalarNode:
			if cs.Command.Value != "" {
				svc.Command = []string{ip.expand(cs.Command.Value)}
			}
		case yaml.SequenceNode:
			for _, n := range cs.Command.Content {
				svc.Command = append(svc.Command, ip.expand(n.Value))
			}
		}
		plan.Services = append(plan.Services, svc)
	}
	rank := func(s Service) int {
		switch {
		case s.Name == primary:
			return 0
		case !s.Private:
			return 1
		}
		return 2
	}
	sort.SliceStable(plan.Services, func(i, j int) bool {
		// The entry point first (it takes a custom domain), then public services.
		return rank(plan.Services[i]) < rank(plan.Services[j])
	})
	for _, v := range ip.missing() {
		warn("%s is not set; set it in the service's environment", v)
	}
	if len(plan.Services) == 0 {
		return nil, fmt.Errorf("no services to create: %s", strings.Join(plan.Warnings, "; "))
	}
	return plan, nil
}

// DefaultBuildPort is the port of an internal service whose compose entry names
// none; the Dockerfile's EXPOSE replaces it at the first build.
const DefaultBuildPort = 8080

func isBuild(cs composeService) bool { return cs.Build.Kind != 0 }

func isDependency(name string, all map[string]composeService) bool {
	for other, cs := range all {
		if other == name {
			continue
		}
		switch cs.DependsOn.Kind {
		case yaml.SequenceNode:
			for _, n := range cs.DependsOn.Content {
				if n.Value == name {
					return true
				}
			}
		case yaml.MappingNode:
			for i := 0; i < len(cs.DependsOn.Content); i += 2 {
				if cs.DependsOn.Content[i].Value == name {
					return true
				}
			}
		}
	}
	return false
}

// composeBuild resolves a build section to a directory and Dockerfile in the repository.
func composeBuild(n yaml.Node, dir string, ip *interpolator) (*Build, error) {
	ctx, dockerfile := ".", ""
	switch n.Kind {
	case yaml.ScalarNode:
		ctx = ip.expand(n.Value)
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			switch k {
			case "context":
				ctx = ip.expand(v.Value)
			case "dockerfile":
				dockerfile = ip.expand(v.Value)
			case "target":
				return nil, fmt.Errorf("multi-stage build targets are not supported")
			case "dockerfile_inline":
				return nil, fmt.Errorf("inline Dockerfiles are not supported")
			}
		}
	default:
		return nil, fmt.Errorf("invalid build section")
	}
	if strings.Contains(ctx, "://") || strings.HasPrefix(ctx, "git@") {
		return nil, fmt.Errorf("remote build contexts are not supported")
	}
	if path.IsAbs(ctx) {
		return nil, fmt.Errorf("absolute build context %s", ctx)
	}
	root := path.Clean(path.Join(firstNonEmpty(dir, "."), ctx))
	if root == ".." || strings.HasPrefix(root, "../") {
		return nil, fmt.Errorf("build context %s is outside the repository", ctx)
	}
	if dockerfile != "" {
		dockerfile = path.Clean(dockerfile)
		if path.IsAbs(dockerfile) || dockerfile == ".." || strings.HasPrefix(dockerfile, "../") {
			return nil, fmt.Errorf("dockerfile %s must be inside the build context", dockerfile)
		}
	}
	if root == "." {
		root = ""
	}
	return &Build{RootDir: root, Dockerfile: dockerfile}, nil
}

func composeEnv(n yaml.Node, ip *interpolator) map[string]string {
	env := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			env[n.Content[i].Value] = ip.expand(n.Content[i+1].Value)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			k, v, ok := strings.Cut(c.Value, "=")
			if !ok {
				continue // "KEY" alone passes the host's value through
			}
			env[k] = ip.expand(v)
		}
	}
	return env
}

// dbURLRef reports whether v is a connection URL pointing at a database service.
func dbURLRef(v string, dbs map[string]bool) (string, bool) {
	if !strings.Contains(v, "://") {
		return "", false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", false
	}
	if dbs[u.Hostname()] {
		return u.Hostname(), true
	}
	return "", false
}

// dbValueRef rewrites a host, host:port, user, password or database name
// copied from a database service into the matching placeholder.
func dbValueRef(key, v string, dbs map[string]bool, envOf map[string]map[string]string) string {
	if dbs[v] {
		return "${db." + v + ".host}"
	}
	if h, p, ok := strings.Cut(v, ":"); ok && dbs[h] {
		if _, err := strconv.Atoi(p); err == nil {
			return "${db." + h + ".host}:${db." + h + ".port}"
		}
	}
	if len(v) < 3 {
		return v
	}
	names := make([]string, 0, len(dbs))
	for n := range dbs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, db := range names {
		roles := map[string]bool{}
		for k, dv := range envOf[db] {
			if role, ok := dbEnvRoles[k]; ok && dv == v {
				roles[role] = true
			}
		}
		if len(roles) == 0 {
			continue
		}
		// The same value can be both the user and the database name: the
		// variable's own name decides.
		upper := strings.ToUpper(key)
		for _, r := range []struct{ role, hint string }{{"password", "PASS"}, {"user", "USER"}, {"name", "NAME"}, {"name", "DATABASE"}, {"name", "DB"}} {
			if roles[r.role] && strings.Contains(upper, r.hint) {
				return "${db." + db + "." + r.role + "}"
			}
		}
		for _, role := range []string{"password", "user", "name"} {
			if roles[role] {
				return "${db." + db + "." + role + "}"
			}
		}
	}
	return v
}

// interpolator resolves compose variables (${VAR}, ${VAR:-default},
// ${VAR:?error}, $VAR). There is no .env file on the platform, so unset
// secrets get a random value (the same one everywhere they appear) and unset
// URLs point at the public service.
type interpolator struct {
	values  map[string]string
	unset   map[string]bool
	primary string
}

func newInterpolator() *interpolator {
	return &interpolator{values: map[string]string{}, unset: map[string]bool{}}
}

var composeVar = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

var (
	secretVar = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|_KEY$|^KEY$|SALT)`)
	urlVar    = regexp.MustCompile(`(?i)(ORIGIN|_URL$|^URL$|PUBLIC_HOST|BASE_URL)`)
)

func (ip *interpolator) expand(s string) string {
	return composeVar.ReplaceAllStringFunc(s, func(m string) string {
		if m == "$$" {
			return "$"
		}
		sub := composeVar.FindStringSubmatch(m)
		name, op, arg := sub[1], sub[2], sub[3]
		if name == "" {
			name = sub[4]
		}
		switch op {
		case ":-", "-":
			return ip.expand(arg)
		case ":+", "+":
			return "" // set only when VAR is set, and nothing is
		}
		return ip.value(name)
	})
}

func (ip *interpolator) value(name string) string {
	if v, ok := ip.values[name]; ok {
		return v
	}
	var v string
	switch {
	case secretVar.MatchString(name):
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		v = hex.EncodeToString(b)
	case urlVar.MatchString(name) && ip.primary != "":
		v = "${svc." + ip.primary + ".url}"
	default:
		ip.unset[name] = true
	}
	ip.values[name] = v
	return v
}

func (ip *interpolator) missing() []string {
	out := make([]string, 0, len(ip.unset))
	for k := range ip.unset {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// containerPort extracts the container side of "8080:80", "127.0.0.1:8080:80/tcp" or "80".
func containerPort(p string) int {
	p = strings.Split(p, "/")[0]
	parts := strings.Split(p, ":")
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0
	}
	return n
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "data"
	}
	return b.String()
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func dependsOn(n yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.SequenceNode:
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			out = append(out, n.Content[i].Value)
		}
	}
	sort.Strings(out)
	return out
}

// envFilePaths lists a service's env_file entries (string, list, or list of {path}).
func envFilePaths(n yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.ScalarNode:
		out = append(out, n.Value)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			switch c.Kind {
			case yaml.ScalarNode:
				out = append(out, c.Value)
			case yaml.MappingNode:
				for i := 0; i+1 < len(c.Content); i += 2 {
					if c.Content[i].Value == "path" {
						out = append(out, c.Content[i+1].Value)
					}
				}
			}
		}
	}
	return out
}

// EnvFileCandidates are the repository files that can stand in for env file
// ref of a compose file in dir: the file itself (rarely committed), then its
// example variants.
func EnvFileCandidates(dir, ref string) []string {
	p := path.Clean(path.Join(firstNonEmpty(dir, "."), ref))
	d := path.Dir(p)
	out := []string{p}
	for _, suf := range []string{".example", ".sample", ".template", ".dist", ".defaults"} {
		out = append(out, p+suf)
	}
	return append(out, path.Join(d, ".env.example"), path.Join(d, ".env.sample"), path.Join(d, "example.env"))
}

func (opt ComposeOptions) envFile(ref string) (content, from string, example bool) {
	for i, c := range EnvFileCandidates(opt.Dir, ref) {
		if v, ok := opt.Files[c]; ok {
			return v, c, i > 0
		}
	}
	return "", "", false
}

type dotenvEntry struct{ key, value, comment string }

// ParseDotenv reads KEY=value lines (export prefix, quotes and trailing
// comments allowed).
func ParseDotenv(content string) [][2]string {
	var out [][2]string
	for _, e := range parseDotenv(content) {
		out = append(out, [2]string{e.key, e.value})
	}
	return out
}

// parseDotenv also keeps the comment block right above each variable
// (a blank line ends it), or the comment after its value.
func parseDotenv(content string) []dotenvEntry {
	var out []dotenvEntry
	var comment []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			comment = nil
			continue
		}
		if strings.HasPrefix(line, "#") {
			if c := strings.TrimSpace(strings.TrimLeft(line, "#")); c != "" && !strings.Contains(c, "=") {
				comment = append(comment, c)
			}
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || !envKey.MatchString(k) {
			comment = nil
			continue
		}
		v = strings.TrimSpace(v)
		trailing := ""
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
			if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
				v = v[1 : end+1]
			}
		} else if i := strings.Index(v, " #"); i >= 0 {
			trailing = strings.TrimSpace(v[i+2:])
			v = strings.TrimSpace(v[:i])
		}
		hint := strings.Join(comment, " ")
		if trailing != "" {
			hint = trailing
		}
		out = append(out, dotenvEntry{key: k, value: v, comment: hint})
	}
	return out
}

var (
	envKey           = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	selfSecret       = regexp.MustCompile(`(?i)^(JWT|SESSION|COOKIE|AUTH|NEXTAUTH|ENCRYPTION|SIGNING|CSRF|APP|SECRET)[A-Z0-9_]*(SECRET|_KEY|_SALT)$|^SECRET(_KEY)?(_BASE)?$|^SALT$`)
	placeholderValue = regexp.MustCompile(`(?i)your[-_ ]|change[-_ ]?(this|me|it)|replace[-_ ]?me|example\.(com|org)|xxxx|^<[^>]+>$|^\.\.\.$|_here$|-here$`)
	dbSchemes        = map[string]string{"postgres": "postgres", "postgresql": "postgres", "mysql": "mysql", "mariadb": "mariadb", "mongodb": "mongo", "mongodb+srv": "mongo", "redis": "redis", "rediss": "redis"}
)

func localHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "0.0.0.0" || h == "host.docker.internal"
}

// EnvHint is a variable of an example env file and what the platform makes of it.
type EnvHint struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`            // value | generated | required | optional | database | url
	Value string `json:"value,omitempty"` // value, generated secret, placeholder (required), engine (database) or path (url)
	Hint  string `json:"hint,omitempty"`  // the comment above it in the file
}

// EnvInput is a variable the user has to (required) or may (optional) fill in.
type EnvInput struct {
	Key         string `json:"key"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	Hint        string `json:"hint,omitempty"`
	From        string `json:"from,omitempty"` // the example file it comes from
}

// ExampleEnv classifies the variables of an example env file: defaults are
// kept, app secrets generated, credentials of other services (API keys,
// OAuth clients) and placeholders must be filled in, empty ones may be,
// database URLs call for a database and localhost URLs for the public URL.
func ExampleEnv(content string) []EnvHint {
	var out []EnvHint
	for _, e := range parseDotenv(content) {
		h := EnvHint{Key: e.key, Hint: e.comment}
		v := e.value
		if u, err := url.Parse(v); err == nil && u.Host != "" {
			if engine, ok := dbSchemes[u.Scheme]; ok {
				h.Kind, h.Value = "database", engine
				out = append(out, h)
				continue
			}
			if localHost(u.Hostname()) && (u.Scheme == "http" || u.Scheme == "https") {
				rest := u.EscapedPath()
				if u.RawQuery != "" {
					rest += "?" + u.RawQuery
				}
				h.Kind, h.Value = "url", strings.TrimSuffix(rest, "/")
				out = append(out, h)
				continue
			}
		}
		switch {
		case selfSecret.MatchString(e.key):
			b := make([]byte, 24)
			_, _ = rand.Read(b)
			h.Kind, h.Value = "generated", hex.EncodeToString(b)
		case v == "":
			h.Kind = "optional"
		case secretVar.MatchString(e.key) || placeholderValue.MatchString(v):
			h.Kind, h.Value = "required", v
		default:
			h.Kind, h.Value = "value", v
		}
		out = append(out, h)
	}
	return out
}

// fileEnv turns an env file into service variables. Database URLs pointing at
// the developer's machine (or written in an example file) become managed
// databases; an example file is read with ExampleEnv, and what it cannot fill
// becomes inputs for the user.
func fileEnv(content string, example bool, primary string, plan *ComposePlan, dbs map[string]bool) (env, links map[string]string, inputs []EnvInput) {
	env, links = map[string]string{}, map[string]string{}
	if !example {
		for _, kv := range ParseDotenv(content) {
			k, v := kv[0], kv[1]
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				if engine, ok := dbSchemes[u.Scheme]; ok && localHost(u.Hostname()) {
					links[k] = ensureDB(plan, dbs, engine)
					continue
				}
			}
			if v != "" {
				env[k] = v
			}
		}
		return env, links, nil
	}
	for _, h := range ExampleEnv(content) {
		switch h.Kind {
		case "database":
			links[h.Key] = ensureDB(plan, dbs, h.Value)
		case "url":
			if primary != "" {
				env[h.Key] = "${svc." + primary + ".url}" + h.Value
			}
		case "generated", "value":
			env[h.Key] = h.Value
		case "required":
			inputs = append(inputs, EnvInput{Key: h.Key, Required: true, Placeholder: h.Value, Hint: h.Hint})
		case "optional":
			inputs = append(inputs, EnvInput{Key: h.Key, Hint: h.Hint})
		}
	}
	return env, links, inputs
}

// ensureDB returns the managed database of an engine, adding one to the plan
// when the compose file has none.
func ensureDB(plan *ComposePlan, dbs map[string]bool, engine string) string {
	for _, d := range plan.Databases {
		if d.Engine == engine {
			return d.Name
		}
	}
	name := engine
	for dbs[name] {
		name += "-db"
	}
	dbs[name] = true
	plan.Databases = append(plan.Databases, Database{Name: name, Engine: engine})
	return name
}

// ComposeEnvFiles lists the repository files that may hold the env files of a
// compose file in dir (see EnvFileCandidates), for the build node to send along.
func ComposeEnvFiles(data []byte, dir string) []string {
	var f composeFile
	if yaml.Unmarshal(data, &f) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, cs := range f.Services {
		for _, ref := range envFilePaths(cs.EnvFile) {
			for _, c := range EnvFileCandidates(dir, ref) {
				if !seen[c] {
					seen[c] = true
					out = append(out, c)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
