package statebackup

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	for _, size := range []int{0, 10, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 5} {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		var sealed bytes.Buffer
		if err := Seal(&sealed, bytes.NewReader(data), key); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := Open(&out, bytes.NewReader(sealed.Bytes()), key); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("size %d: round trip differs", size)
		}
		if size > chunkSize {
			// Dropping the last chunk must be detected.
			cut := sealed.Bytes()[:sealed.Len()-(size%chunkSize+16+4)]
			if err := Open(&bytes.Buffer{}, bytes.NewReader(cut), key); err == nil {
				t.Fatalf("size %d: truncated backup opened", size)
			}
		}
		if err := Open(&bytes.Buffer{}, bytes.NewReader(sealed.Bytes()), bytes.Repeat([]byte{8}, 32)); err == nil {
			t.Fatal("opened with the wrong key")
		}
	}
}
