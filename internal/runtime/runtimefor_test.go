package runtime

import (
	"testing"

	"github.com/thesunbg/paas/internal/model"
)

func TestRuntimeFor(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no containerd-shim-runsc-v1
	r := &Runtime{ociBinary: "crun"}
	cases := []struct {
		class   string
		want    string
		wantErr bool
	}{
		{model.RuntimeStandard, "io.containerd.runc.v2", false},
		{"", "io.containerd.runc.v2", false},
		{model.RuntimeSandboxed, "", true},
	}
	for _, c := range cases {
		got, _, err := r.runtimeFor(c.class)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("runtimeFor(%q) = %q, %v; want %q, err=%v", c.class, got, err, c.want, c.wantErr)
		}
	}
}
