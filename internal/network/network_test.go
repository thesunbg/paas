package network

import (
	"flag"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func TestRulesetGolden(t *testing.T) {
	for _, c := range []struct {
		file      string
		shared    int
		resolvers []string
	}{
		{"testdata/ruleset.golden", 5, []string{"10.0.0.2"}},
		{"testdata/ruleset_noshared.golden", 0, nil},
	} {
		got := ruleset([2]int{10, 100}, 1, c.shared, c.resolvers)
		if *update {
			if err := os.MkdirAll("testdata", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(c.file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from ruleset(); run go test -update and review the diff", c.file)
		}
	}
}

// nft -c catches syntax errors that a golden comparison cannot.
func TestRulesetNftCheck(t *testing.T) {
	for _, bin := range []string{"nft", "unshare"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	for _, rules := range []string{
		ruleset([2]int{10, 100}, 1, 5, []string{"10.0.0.2", "192.168.1.1"}),
		ruleset([2]int{10, 100}, 1, 0, nil),
	} {
		cmd := exec.Command("unshare", "-rn", "nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(rules)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "unshare failed") {
				t.Skipf("cannot check ruleset here: %s", out)
			}
			t.Fatalf("nft -c: %v\n%s", err, out)
		}
	}
}

func TestPrivateResolvers(t *testing.T) {
	in := []string{"10.0.0.2:53", "127.0.0.53:53", "8.8.8.8:53", "[fd00::1]:53", "172.16.5.1:53", "172.32.0.1:53",
		"192.168.1.1:53", "100.64.0.2:53", "100.128.0.1:53", "169.254.169.253:53", "1.1.1.1"}
	want := []string{"10.0.0.2", "172.16.5.1", "192.168.1.1", "100.64.0.2", "169.254.169.253"}
	if got := privateResolvers(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
