package server

import (
	"testing"

	"github.com/thesunbg/paas/internal/model"
)

func TestDesiredReplicas(t *testing.T) {
	a := model.Autoscale{Enabled: true, Min: 1, Max: 6, TargetCPUPercent: 70, TargetRPS: 50}
	cases := []struct {
		cur      int
		cpu, rps float64
		want     int
	}{
		{2, 140, 10, 4}, // cpu doubled the target
		{2, 30, 120, 5}, // rps per replica 2.4x target
		{3, 60, 40, 3},  // within band
		{4, 10, 5, 1},   // very low -> shrink (clamped to min)
		{5, 400, 0, 6},  // clamp to max
	}
	for _, c := range cases {
		if got := desiredReplicas(a, c.cur, c.cpu, c.rps); got != c.want {
			t.Errorf("cur=%d cpu=%.0f rps=%.0f: got %d want %d", c.cur, c.cpu, c.rps, got, c.want)
		}
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	// echo -n '{"ref":"refs/heads/main"}' | openssl dgst -sha256 -hmac s3cret
	sig := "sha256=" + hmacHex("s3cret", body)
	if !verifyGitHubSignature("s3cret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	if verifyGitHubSignature("other", body, sig) || verifyGitHubSignature("s3cret", body, "sha256=00") {
		t.Fatal("invalid signature accepted")
	}
}
