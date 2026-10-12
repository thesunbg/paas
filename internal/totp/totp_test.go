package totp

import (
	"testing"
	"time"
)

// RFC 6238 test vector (SHA1, secret "12345678901234567890").
func TestRFCVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	code, err := Code(secret, time.Unix(59, 0))
	if err != nil || code != "287082" {
		t.Fatalf("got %s %v", code, err)
	}
	if !Verify(secret, "287082", time.Unix(59+25, 0)) {
		t.Fatal("adjacent step should verify")
	}
	if Verify(secret, "287082", time.Unix(59+120, 0)) {
		t.Fatal("old code must fail")
	}
}
