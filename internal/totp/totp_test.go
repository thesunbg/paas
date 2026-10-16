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
	if _, ok := Match(secret, "287082", time.Unix(59+120, 0)); ok {
		t.Fatal("old code must fail")
	}
}

func TestMatchStep(t *testing.T) {
	secret := NewSecret()
	now := time.Unix(1_700_000_010, 0)
	for _, d := range []int64{-1, 0, 1} {
		code, _ := Code(secret, now.Add(time.Duration(d)*30*time.Second))
		step, ok := Match(secret, code, now)
		if !ok || step != now.Unix()/30+d {
			t.Errorf("window %d: step=%d ok=%v, want %d", d, step, ok, now.Unix()/30+d)
		}
	}
	code, _ := Code(secret, now)
	wrong := "000000"
	if code == wrong {
		wrong = "000001"
	}
	if _, ok := Match(secret, wrong, now); ok {
		t.Error("wrong code matched")
	}
	far, _ := Code(secret, now.Add(5*time.Minute))
	if _, ok := Match(secret, far, now); ok {
		t.Error("code outside the window matched")
	}
}
