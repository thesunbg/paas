// Package secrets encrypts values at rest (env vars, git tokens, DB passwords) with AES-256-GCM.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const prefix = "v1:"

type Box struct {
	aead cipher.AEAD
}

// New creates a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// LoadOrCreateKey reads a hex-encoded key from path, generating one (mode 0600) if missing.
// If envKey is non-empty it takes precedence.
func LoadOrCreateKey(path, envKey string) ([]byte, error) {
	if envKey != "" {
		return decodeKey(envKey)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(strings.TrimSpace(string(data)))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// LoadKey reads an existing key (envKey first, then path); unlike
// LoadOrCreateKey it never makes a new one.
func LoadKey(path, envKey string) ([]byte, error) {
	if envKey != "" {
		return decodeKey(envKey)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := decodeKey(strings.TrimSpace(string(data)))
	if err == nil && len(k) != 32 {
		err = fmt.Errorf("master key must be 32 bytes, got %d", len(k))
	}
	return k, err
}

func decodeKey(s string) ([]byte, error) {
	k, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("master key must be hex: %w", err)
	}
	return k, nil
}

func (b *Box) Encrypt(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(out), nil
}

func (b *Box) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	if !strings.HasPrefix(enc, prefix) {
		return "", errors.New("unknown ciphertext format")
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc[len(prefix):])
	if err != nil {
		return "", err
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// MustEncrypt panics on failure; only randomness failures can make Encrypt fail.
func (b *Box) MustEncrypt(plain string) string {
	s, err := b.Encrypt(plain)
	if err != nil {
		panic(err)
	}
	return s
}
