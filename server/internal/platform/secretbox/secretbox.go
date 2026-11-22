// Package secretbox encrypts tenant secrets at rest with AES-256-GCM (PLAN section 6).
//
// Format: version (1 byte) || nonce (12 bytes) || ciphertext+tag. The version byte leaves room for a key rotation
// later (a second key tried on version 2). Every value is sealed with a context string, used as associated data,
// such as "tenant/<id>/api_secret": a ciphertext copied into another row or column fails to open.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

const version1 byte = 1

var ErrDecrypt = errors.New("secretbox: cannot decrypt (wrong key, wrong context or corrupted value)")

type Box struct {
	aead cipher.AEAD
}

// New takes the 32 byte master key (config.Secrets.MasterKeyBytes).
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext bound to context.
func (b *Box) Seal(plaintext []byte, context string) []byte {
	nonceSize := b.aead.NonceSize()
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+b.aead.Overhead())
	out[0] = version1
	if _, err := rand.Read(out[1 : 1+nonceSize]); err != nil {
		panic("secretbox: crypto/rand failed: " + err.Error()) // never happens on supported platforms
	}
	return b.aead.Seal(out, out[1:1+nonceSize], plaintext, []byte(context)) //nolint:gosec // G407 false positive: the nonce is filled by crypto/rand above
}

// Open decrypts a value produced by Seal with the same context.
func (b *Box) Open(sealed []byte, context string) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(sealed) < 1+nonceSize+b.aead.Overhead() || sealed[0] != version1 {
		return nil, ErrDecrypt
	}
	plain, err := b.aead.Open(nil, sealed[1:1+nonceSize], sealed[1+nonceSize:], []byte(context))
	if err != nil {
		return nil, ErrDecrypt
	}
	return plain, nil
}
