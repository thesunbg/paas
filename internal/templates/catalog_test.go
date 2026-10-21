package templates

import (
	"regexp"
	"strings"
	"testing"

	"github.com/thesunbg/paas/internal/databases"
	"github.com/thesunbg/paas/internal/resources"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var refRe = regexp.MustCompile(`\$\{([a-z0-9_.:-]+)\}`)

// Every template must install as written: known engines and sizes, fully qualified
// images, and placeholders that point at something the template creates.
func TestCatalog(t *testing.T) {
	ids := map[string]bool{}
	for _, tpl := range All {
		if ids[tpl.ID] {
			t.Errorf("duplicate template id %s", tpl.ID)
		}
		ids[tpl.ID] = true
		if tpl.Title == "" || tpl.Description == "" || tpl.Category == "" || len(tpl.Services) == 0 {
			t.Errorf("%s: title, description, category and a service are required", tpl.ID)
		}
		dbs := map[string]bool{}
		for _, d := range tpl.Databases {
			e, ok := databases.Engines[d.Engine]
			if !ok {
				t.Errorf("%s: unknown engine %s", tpl.ID, d.Engine)
				continue
			}
			if !e.SharedOK && d.Mode != "dedicated" {
				t.Errorf("%s: %s cannot be shared, set Mode dedicated", tpl.ID, d.Engine)
			}
			dbs[d.Name] = true
		}
		for _, s := range tpl.Services {
			if s.Build != nil {
				if tpl.Repo == "" {
					t.Errorf("%s/%s: built from source but the template has no repo", tpl.ID, s.Name)
				}
				continue
			}
			if strings.Count(strings.SplitN(s.Image, "/", 2)[0], ".") == 0 || !strings.Contains(s.Image, ":") {
				t.Errorf("%s/%s: image %q must name its registry and tag", tpl.ID, s.Name, s.Image)
			}
			if s.Size != "" {
				if _, ok := resources.SizeByName(s.Size); !ok {
					t.Errorf("%s/%s: unknown size %s", tpl.ID, s.Name, s.Size)
				}
			}
			keys := map[string]bool{}
			for k, v := range s.Env {
				keys[k] = true
				if !envName.MatchString(k) {
					t.Errorf("%s/%s: %s is not a valid variable name (the Environment tab could not edit it)", tpl.ID, s.Name, k)
				}
				for _, m := range refRe.FindAllStringSubmatch(v, -1) {
					ref := m[1]
					switch {
					case ref == "url" || ref == "host" || ref == "random" || regexp.MustCompile(`^random:[0-9]+$`).MatchString(ref):
					case strings.HasPrefix(ref, "db."):
						p := strings.Split(ref, ".")
						if len(p) != 3 || !dbs[p[1]] {
							t.Errorf("%s/%s: %s refers to %s, which the template does not create", tpl.ID, s.Name, k, ref)
						}
					default:
						t.Errorf("%s/%s: %s has unknown placeholder %s", tpl.ID, s.Name, k, ref)
					}
				}
			}
			for _, in := range s.Inputs {
				if keys[in.Key] {
					t.Errorf("%s/%s: input %s is also a fixed variable", tpl.ID, s.Name, in.Key)
				}
			}
			vols := map[string]bool{}
			for _, v := range s.Volumes {
				if vols[v.Name] || !strings.HasPrefix(v.Path, "/") {
					t.Errorf("%s/%s: bad or duplicate volume %v", tpl.ID, s.Name, v)
				}
				vols[v.Name] = true
			}
		}
	}
	if len(All) < 30 {
		t.Errorf("only %d templates", len(All))
	}
}
