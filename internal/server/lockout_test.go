package server

import (
	"fmt"
	"testing"
)

func TestLockoutPerEmailIP(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "victim@example.com", "password1", false)
	for i := 0; i < maxPerEmailIP; i++ {
		if code, _ := login(t, h, "victim@example.com", "wrong", "198.51.100.1"); code != 401 {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code, _ := login(t, h, "victim@example.com", "password1", "198.51.100.1"); code != 429 {
		t.Errorf("locked IP = %d, want 429", code)
	}
	if code, _ := login(t, h, "victim@example.com", "password1", "198.51.100.2"); code != 200 {
		t.Errorf("other IP = %d, want 200", code)
	}
}

func TestLockoutPerIP(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "fresh@example.com", "password1", false)
	for i := 0; i < maxPerIP; i++ {
		login(t, h, fmt.Sprintf("u%d@example.com", i), "wrong", "198.51.100.1")
	}
	if code, _ := login(t, h, "fresh@example.com", "password1", "198.51.100.1"); code != 429 {
		t.Errorf("spraying IP = %d, want 429", code)
	}
}

func TestLockoutPerEmailCap(t *testing.T) {
	s, h := newTestServer(t)
	mkUser(t, s, "victim@example.com", "password1", false)
	for i := 0; i < maxPerEmail; i++ {
		login(t, h, "victim@example.com", "wrong", fmt.Sprintf("198.51.%d.%d", i/200, i%200+1))
	}
	if code, _ := login(t, h, "victim@example.com", "password1", "203.0.113.9"); code != 429 {
		t.Errorf("new IP after distributed attack = %d, want 429", code)
	}
}
