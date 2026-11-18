// Package crypto encrypts integration credentials at rest with envelope
// encryption: every record gets its own random data key (DEK) that encrypts
// the value, and the DEK is itself encrypted with the master key (KEK).
//
// The additional authenticated data (AAD, here the record id) is bound into
// both layers, so a ciphertext copied to another row fails to decrypt.
//
// Blob layout: version(1) | nonce(12) | wrapped DEK (32+16) | nonce(12) | ciphertext+tag
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	version   = 1
	keySize   = 32
	nonceSize = 12
)

// Box seals and opens values with a master key.
type Box struct{ kek cipher.AEAD }

// NewBox builds a Box from a base64 (standard or URL-safe) encoded 32-byte key.
func NewBox(b64 string) (*Box, error) {
	key, err := decodeKey(b64)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &Box{kek: aead}, nil
}

// GenerateKey returns a new random master key, base64 encoded.
func GenerateKey() (string, error) {
	k := make([]byte, keySize)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil {
			if len(k) != keySize {
				return nil, fmt.Errorf("crypto: master key must be %d bytes, got %d", keySize, len(k))
			}
			return k, nil
		}
	}
	return nil, errors.New("crypto: master key is not valid base64")
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal encrypts plaintext, binding it to aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	n1, n2 := make([]byte, nonceSize), make([]byte, nonceSize)
	if _, err := rand.Read(n1); err != nil {
		return nil, err
	}
	if _, err := rand.Read(n2); err != nil {
		return nil, err
	}
	out := []byte{version}
	out = append(out, n1...)
	out = b.kek.Seal(out, n1, dek, aad)
	out = append(out, n2...)
	out = dekAEAD.Seal(out, n2, plaintext, aad)
	return out, nil
}

// Open decrypts a blob produced by Seal with the same aad.
func (b *Box) Open(blob, aad []byte) ([]byte, error) {
	wrappedLen := keySize + b.kek.Overhead()
	if len(blob) < 1+nonceSize+wrappedLen+nonceSize+16 || blob[0] != version {
		return nil, errors.New("crypto: malformed or unsupported ciphertext")
	}
	pos := 1
	n1 := blob[pos : pos+nonceSize]
	pos += nonceSize
	dek, err := b.kek.Open(nil, n1, blob[pos:pos+wrappedLen], aad)
	if err != nil {
		return nil, errors.New("crypto: cannot decrypt (wrong master key or tampered data)")
	}
	pos += wrappedLen
	n2 := blob[pos : pos+nonceSize]
	pos += nonceSize
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	pt, err := dekAEAD.Open(nil, n2, blob[pos:], aad)
	if err != nil {
		return nil, errors.New("crypto: cannot decrypt (wrong master key or tampered data)")
	}
	return pt, nil
}
