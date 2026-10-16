package netguard

import (
	"net"
	"testing"
)

func TestPublic(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.0.0.1": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "0.0.0.0": false, "224.0.0.1": false, "100.64.0.1": false, "fe80::1": false,
	} {
		if got := Public(net.ParseIP(ip)); got != want {
			t.Errorf("Public(%s) = %v, want %v", ip, got, want)
		}
	}
	if Public(nil) {
		t.Error("nil must not be public")
	}
}
