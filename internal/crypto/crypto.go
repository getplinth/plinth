// Package crypto encrypts integration credentials at rest with envelope
// encryption: every record gets its own random data key (DEK) that encrypts
// the value, and the DEK is itself encrypted with the master key (KEK).
//
// The additional authenticated data (AAD, here the record id) is bound into
// both layers, so a ciphertext copied to another row fails to decrypt.
//
// Master keys can be rotated. A Box holds one primary key, which encrypts
// everything new, and any number of previous keys that can still decrypt.
// Because only the small DEK is wrapped by the master key, Rewrap moves a
// record to the primary key without touching the encrypted value.
//
// Blob layout (version 2): version(1) | key id(4) | nonce(12) | wrapped DEK (32+16) | nonce(12) | ciphertext+tag
// Version 1 blobs have no key id; they are tried against every key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	versionLegacy = 1 // no key id
	version       = 2
	keySize       = 32
	nonceSize     = 12
	idSize        = 4
)

type masterKey struct {
	id   [idSize]byte
	aead cipher.AEAD
}

// Box seals and opens values with master keys.
type Box struct {
	keys []masterKey // keys[0] is the primary
}

// NewBox builds a Box from one base64 (standard or URL-safe) encoded 32-byte key.
func NewBox(b64 string) (*Box, error) { return NewRing(b64) }

// NewRing builds a Box that encrypts with primary and can also decrypt what
// was encrypted with any of the previous keys. Use it while rotating the
// master key. The same key twice is refused, since that is almost certainly a
// mistake.
func NewRing(primary string, previous ...string) (*Box, error) {
	b := &Box{}
	for i, enc := range append([]string{primary}, previous...) {
		name := "master key"
		if i > 0 {
			name = fmt.Sprintf("previous master key %d", i)
		}
		key, err := decodeKey(enc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		aead, err := newGCM(key)
		if err != nil {
			return nil, err
		}
		mk := masterKey{id: keyID(key), aead: aead}
		for _, other := range b.keys {
			if other.id == mk.id {
				return nil, fmt.Errorf("%s is the same as an earlier key", name)
			}
		}
		b.keys = append(b.keys, mk)
	}
	return b, nil
}

// keyID is a short public fingerprint of a key, used to tell keys apart. It
// does not reveal the key.
func keyID(key []byte) [idSize]byte {
	h := sha256.Sum256(append([]byte("plinth-key-id:"), key...))
	var id [idSize]byte
	copy(id[:], h[:idSize])
	return id
}

// PrimaryID is the fingerprint of the key that encrypts new values.
func (b *Box) PrimaryID() string { return hex.EncodeToString(b.keys[0].id[:]) }

// KeyIDs lists the fingerprints of all configured keys, primary first.
func (b *Box) KeyIDs() []string {
	out := make([]string, len(b.keys))
	for i, k := range b.keys {
		out[i] = hex.EncodeToString(k.id[:])
	}
	return out
}

// KeyOf says which key a blob is encrypted with: the fingerprint, or "" for a
// blob from before keys carried an id (it is tried against every key).
func KeyOf(blob []byte) (id string, ok bool) {
	if len(blob) == 0 {
		return "", false
	}
	switch blob[0] {
	case version:
		if len(blob) < 1+idSize {
			return "", false
		}
		return hex.EncodeToString(blob[1 : 1+idSize]), true
	case versionLegacy:
		return "", true
	}
	return "", false
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

// Seal encrypts plaintext with the primary key, binding it to aad.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	n2 := make([]byte, nonceSize)
	if _, err := rand.Read(n2); err != nil {
		return nil, err
	}
	out, err := b.wrap(dek, aad)
	if err != nil {
		return nil, err
	}
	out = append(out, n2...)
	return dekAEAD.Seal(out, n2, plaintext, aad), nil
}

// wrap starts a version 2 blob: header, key id, and the DEK wrapped by the primary key.
func (b *Box) wrap(dek, aad []byte) ([]byte, error) {
	n1 := make([]byte, nonceSize)
	if _, err := rand.Read(n1); err != nil {
		return nil, err
	}
	k := b.keys[0]
	out := append([]byte{version}, k.id[:]...)
	out = append(out, n1...)
	return k.aead.Seal(out, n1, dek, aad), nil
}

var errDecrypt = errors.New("crypto: cannot decrypt (wrong master key or tampered data)")

// unwrap recovers the DEK and returns the rest of the blob (nonce + ciphertext)
// and the position of the key that unlocked it.
func (b *Box) unwrap(blob, aad []byte) (dek, rest []byte, keyIndex int, err error) {
	if len(blob) == 0 || (blob[0] != version && blob[0] != versionLegacy) {
		return nil, nil, 0, errors.New("crypto: malformed or unsupported ciphertext")
	}
	pos := 1
	var candidates []int
	if blob[0] == version {
		if len(blob) < 1+idSize {
			return nil, nil, 0, errors.New("crypto: malformed or unsupported ciphertext")
		}
		for i, k := range b.keys {
			if string(k.id[:]) == string(blob[1:1+idSize]) {
				candidates = []int{i}
			}
		}
		if candidates == nil {
			return nil, nil, 0, fmt.Errorf("crypto: encrypted with key %s, which is not configured: add it to PLINTH_MASTER_KEY_PREVIOUS", hex.EncodeToString(blob[1:1+idSize]))
		}
		pos += idSize
	} else {
		for i := range b.keys {
			candidates = append(candidates, i)
		}
	}
	wrappedLen := keySize + b.keys[0].aead.Overhead()
	if len(blob) < pos+nonceSize+wrappedLen+nonceSize+16 {
		return nil, nil, 0, errors.New("crypto: malformed or unsupported ciphertext")
	}
	n1 := blob[pos : pos+nonceSize]
	wrapped := blob[pos+nonceSize : pos+nonceSize+wrappedLen]
	for _, i := range candidates {
		if dek, err := b.keys[i].aead.Open(nil, n1, wrapped, aad); err == nil {
			return dek, blob[pos+nonceSize+wrappedLen:], i, nil
		}
	}
	return nil, nil, 0, errDecrypt
}

// Open decrypts a blob produced by Seal with the same aad.
func (b *Box) Open(blob, aad []byte) ([]byte, error) {
	dek, rest, _, err := b.unwrap(blob, aad)
	if err != nil {
		return nil, err
	}
	dekAEAD, err := newGCM(dek)
	if err != nil {
		return nil, err
	}
	pt, err := dekAEAD.Open(nil, rest[:nonceSize], rest[nonceSize:], aad)
	if err != nil {
		return nil, errDecrypt
	}
	return pt, nil
}

// Rewrap moves a blob to the primary key without decrypting the value: only
// the data key is unwrapped and wrapped again. changed is false when the blob
// already uses the primary key (the same blob is returned). The value is
// verified to still decrypt before the new blob is returned, so a bad rewrap
// never replaces a good record.
func (b *Box) Rewrap(blob, aad []byte) (out []byte, changed bool, err error) {
	dek, rest, idx, err := b.unwrap(blob, aad)
	if err != nil {
		return nil, false, err
	}
	if idx == 0 && blob[0] == version {
		return blob, false, nil
	}
	out, err = b.wrap(dek, aad)
	if err != nil {
		return nil, false, err
	}
	out = append(out, rest...)
	if _, err := b.Open(out, aad); err != nil {
		return nil, false, fmt.Errorf("crypto: rewrapped record does not verify: %w", err)
	}
	return out, true, nil
}
