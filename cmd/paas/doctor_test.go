package main

import (
	"strings"
	"testing"
)

func TestPrintDoctor(t *testing.T) {
	rep := doctorReport{Steps: []doctorStep{
		{ID: "admin", Title: "Admin account", Status: "ok", Summary: "Signed in"},
		{ID: "dns", Title: "Domain & DNS", Status: "fail", Summary: "missing", HowTo: "create records",
			Records: []doctorRecord{{Name: "*.apps.test", Type: "A", Value: "203.0.113.10"}}},
		{ID: "github", Title: "Git", Status: "fail", Summary: "none", Skipped: true},
		{ID: "backups", Title: "Off-site backups", Status: "warn", Summary: "off"},
	}}
	var b strings.Builder
	if n := printDoctor(&b, "https://paas.apps.test", rep, false); n != 1 {
		t.Fatalf("failed = %d, want 1 (skipped steps do not count)", n)
	}
	out := b.String()
	for _, want := range []string{"[ ok ] 1. Admin account", "[FAIL] 2. Domain & DNS", "*.apps.test", "203.0.113.10", "How to: create records", "(skipped)", "1 ok, 1 warnings, 1 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
