package crypto

import (
	"bytes"
	"strings"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBox(k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t)
	for _, pt := range [][]byte{[]byte("hello"), {}, bytes.Repeat([]byte("x"), 100_000)} {
		blob, err := b.Seal(pt, []byte("id-1"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := b.Open(blob, []byte("id-1"))
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("round trip failed: %v", err)
		}
	}
}

func TestCiphertextHidesPlaintextAndIsRandomised(t *testing.T) {
	b := newBox(t)
	secret := []byte("super-secret-token-value")
	a, _ := b.Seal(secret, []byte("id"))
	c, _ := b.Seal(secret, []byte("id"))
	if bytes.Contains(a, secret) {
		t.Fatal("plaintext visible in ciphertext")
	}
	if bytes.Equal(a, c) {
		t.Fatal("sealing twice must give different ciphertexts")
	}
}

func TestBoundToRecord(t *testing.T) {
	b := newBox(t)
	blob, _ := b.Seal([]byte("secret"), []byte("integration-A"))
	if _, err := b.Open(blob, []byte("integration-B")); err == nil {
		t.Fatal("a ciphertext copied to another record must not decrypt")
	}
}

func TestWrongKeyAndTampering(t *testing.T) {
	b1, b2 := newBox(t), newBox(t)
	blob, _ := b1.Seal([]byte("secret"), []byte("id"))
	if _, err := b2.Open(blob, []byte("id")); err == nil {
		t.Fatal("wrong master key must fail")
	}
	for i := 0; i < len(blob); i++ {
		bad := append([]byte(nil), blob...)
		bad[i] ^= 0x01
		if _, err := b1.Open(bad, []byte("id")); err == nil {
			t.Fatalf("flipping byte %d went unnoticed", i)
		}
	}
	for _, short := range [][]byte{nil, {1}, blob[:10], blob[:len(blob)-1]} {
		if _, err := b1.Open(short, []byte("id")); err == nil {
			t.Fatal("truncated blob must fail")
		}
	}
}

func TestKeyValidation(t *testing.T) {
	for name, k := range map[string]string{"empty": "", "not base64": "!!!", "too short": "c2hvcnQ=", "too long": strings.Repeat("A", 64)} {
		if _, err := NewBox(k); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	k, _ := GenerateKey()
	if _, err := NewBox(k); err != nil {
		t.Fatal(err)
	}
}
