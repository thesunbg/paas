// Package statebackup seals and opens backups of the control plane's own
// database. A sealed file is encrypted and authenticated with a key derived
// from the master key, in 1 MiB chunks so a dump of any size streams:
//
//	"PAASSTATE1\n" | salt (16) | { length (4, big endian) | AES-256-GCM(chunk) }...
//
// Each chunk's nonce is its index; the last chunk is sealed with different
// additional data, so a truncated or reordered file fails to open.
package statebackup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	magic     = "PAASSTATE1\n"
	chunkSize = 1 << 20
)

var (
	adMore = []byte("more")
	adLast = []byte("last")
)

func aeadFor(masterKey, salt []byte) (cipher.AEAD, error) {
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte("paas state backup|"))
	mac.Write(salt)
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], n)
	return b
}

// Seal encrypts r into w.
func Seal(w io.Writer, r io.Reader, masterKey []byte) error {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	aead, err := aeadFor(masterKey, salt)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, magic); err != nil {
		return err
	}
	if _, err := w.Write(salt); err != nil {
		return err
	}
	cur := make([]byte, chunkSize)
	next := make([]byte, chunkSize)
	n, err := io.ReadFull(r, cur)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	for i := uint64(0); ; i++ {
		var m int
		var rerr error
		if n == chunkSize { // read ahead to know whether this chunk is the last
			m, rerr = io.ReadFull(r, next)
			if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
				return rerr
			}
		}
		last := m == 0
		ad := adMore
		if last {
			ad = adLast
		}
		ct := aead.Seal(nil, nonce(i), cur[:n], ad)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if _, err := w.Write(ct); err != nil {
			return err
		}
		if last {
			return nil
		}
		cur, next, n = next, cur, m
	}
}

// Open decrypts a sealed stream from r into w.
func Open(w io.Writer, r io.Reader, masterKey []byte) error {
	head := make([]byte, len(magic)+16)
	if _, err := io.ReadFull(r, head); err != nil {
		return fmt.Errorf("not a state backup: %w", err)
	}
	if string(head[:len(magic)]) != magic {
		return errors.New("not a state backup")
	}
	aead, err := aeadFor(masterKey, head[len(magic):])
	if err != nil {
		return err
	}
	for i := uint64(0); ; i++ {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return errors.New("the backup is truncated")
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > chunkSize+uint32(aead.Overhead()) {
			return errors.New("the backup is corrupt")
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(r, ct); err != nil {
			return errors.New("the backup is truncated")
		}
		if pt, err := aead.Open(nil, nonce(i), ct, adMore); err == nil {
			if _, err := w.Write(pt); err != nil {
				return err
			}
			continue
		}
		pt, err := aead.Open(nil, nonce(i), ct, adLast)
		if err != nil {
			return errors.New("the backup cannot be decrypted: wrong master key or corrupt file")
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		return nil
	}
}
