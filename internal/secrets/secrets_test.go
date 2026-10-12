package secrets

import (
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "k"), "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := b.Encrypt("s3cret value")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "s3cret value" {
		t.Fatal("not encrypted")
	}
	got, err := b.Decrypt(enc)
	if err != nil || got != "s3cret value" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := b.Decrypt(enc[:len(enc)-2] + "AA"); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestKeyPersisted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	k1, _ := LoadOrCreateKey(p, "")
	k2, _ := LoadOrCreateKey(p, "")
	if string(k1) != string(k2) {
		t.Fatal("key should be stable")
	}
}
