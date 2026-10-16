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
}

// dbImages maps database images to managed engines.
var dbImages = map[string]string{
	"postgres": "postgres", "postgis": "postgres", "timescaledb": "postgres",
	"mysql": "mysql", "mariadb": "mysql",
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
	ip.primary = primary
	envOf := map[string]map[string]string{}
	for _, name := range names {
		envOf[name] = composeEnv(f.Services[name].Environment, ip)
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
	sort.SliceStable(plan.Services, func(i, j int) bool {
		// Public services first: the first one takes a custom domain.
		return !plan.Services[i].Private && plan.Services[j].Private
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
