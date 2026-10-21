package agent

import "testing"

func TestNatsURLs(t *testing.T) {
	cases := []struct {
		stored string
		pool   []string
		want   string
	}{
		{"tls://cp1:4222", []string{"tls://cp1:4222"}, "tls://cp1:4222"},
		{"tls://cp1:4222", []string{"tls://cp1:4222", "nats://cp2:4222"}, "tls://cp1:4222,tls://cp2:4222"},
		// Pool order is random; the result is not.
		{"tls://cp1:4222,tls://cp2:4222", []string{"nats://cp2:4222", "tls://cp1:4222"}, "tls://cp1:4222,tls://cp2:4222"},
		// The address the node enrolled with is kept when the cluster stops advertising it.
		{"tls://cp1:4222", []string{"nats://cp2:4222"}, "tls://cp1:4222,tls://cp2:4222"},
		{"tls://cp1:4222", []string{"", "::bad"}, "tls://cp1:4222"},
	}
	for _, c := range cases {
		if got := natsURLs(c.stored, c.pool); got != c.want {
			t.Errorf("natsURLs(%q, %v) = %q, want %q", c.stored, c.pool, got, c.want)
		}
	}
}
