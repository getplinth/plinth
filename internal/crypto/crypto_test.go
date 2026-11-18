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

func key(t *testing.T) string {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// legacySeal produces a version 1 blob, as written before keys carried an id.
func legacySeal(t *testing.T, b64 string, plaintext, aad []byte) []byte {
	t.Helper()
	k, _ := decodeKey(b64)
	kek, _ := newGCM(k)
	dek := bytes.Repeat([]byte{7}, keySize)
	dekAEAD, _ := newGCM(dek)
	n1, n2 := bytes.Repeat([]byte{1}, nonceSize), bytes.Repeat([]byte{2}, nonceSize)
	out := append([]byte{versionLegacy}, n1...)
	out = kek.Seal(out, n1, dek, aad)
	out = append(out, n2...)
	return dekAEAD.Seal(out, n2, plaintext, aad)
}

func TestRotationKeepsOldDataReadableAndNewDataOnNewKey(t *testing.T) {
	oldK, newK := key(t), key(t)
	oldBox, _ := NewBox(oldK)
	blob, _ := oldBox.Seal([]byte("secret"), []byte("id"))

	ring, err := NewRing(newK, oldK)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ring.Open(blob, []byte("id")); err != nil || string(got) != "secret" {
		t.Fatalf("old data must stay readable during rotation: %v", err)
	}
	fresh, _ := ring.Seal([]byte("new"), []byte("id"))
	if id, _ := KeyOf(fresh); id != ring.PrimaryID() {
		t.Errorf("new values must use the primary key: %s vs %s", id, ring.PrimaryID())
	}
	if _, err := oldBox.Open(fresh, []byte("id")); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("the old key alone must not open new data, and should say why: %v", err)
	}
}

func TestRewrapMovesToPrimaryWithoutChangingTheValue(t *testing.T) {
	oldK, newK := key(t), key(t)
	oldBox, _ := NewBox(oldK)
	ring, _ := NewRing(newK, oldK)
	aad := []byte("id")
	blob, _ := oldBox.Seal([]byte("secret"), aad)

	out, changed, err := ring.Rewrap(blob, aad)
	if err != nil || !changed {
		t.Fatalf("rewrap: %v changed=%v", err, changed)
	}
	if id, _ := KeyOf(out); id != ring.PrimaryID() {
		t.Error("rewrapped blob is not on the primary key")
	}
	// Only the wrapped key changed; the encrypted value is byte for byte the same.
	tail := func(b []byte) []byte { return b[len(b)-(nonceSize+len("secret")+16):] }
	if !bytes.Equal(tail(out), tail(blob)) {
		t.Error("the encrypted value must not be touched")
	}
	newOnly, _ := NewBox(newK)
	if got, err := newOnly.Open(out, aad); err != nil || string(got) != "secret" {
		t.Fatalf("after rewrap the new key alone must be enough: %v", err)
	}
	if _, err := newOnly.Open(out, []byte("other")); err == nil {
		t.Error("still bound to its record")
	}
	again, changed, err := ring.Rewrap(out, aad)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Errorf("rewrapping twice must be a no-op: %v %v", err, changed)
	}
	if _, _, err := ring.Rewrap(blob, []byte("wrong")); err == nil {
		t.Error("rewrap must not work for the wrong record")
	}
}

func TestLegacyBlobsAreReadAndUpgraded(t *testing.T) {
	oldK, newK := key(t), key(t)
	blob := legacySeal(t, oldK, []byte("secret"), []byte("id"))
	if id, ok := KeyOf(blob); !ok || id != "" {
		t.Errorf("legacy: %q %v", id, ok)
	}
	plain, _ := NewBox(oldK)
	if got, err := plain.Open(blob, []byte("id")); err != nil || string(got) != "secret" {
		t.Fatalf("legacy blobs must stay readable: %v", err)
	}
	ring, _ := NewRing(newK, oldK)
	if got, err := ring.Open(blob, []byte("id")); err != nil || string(got) != "secret" {
		t.Fatalf("legacy blob under a ring: %v", err)
	}
	// Even when it is already under the primary key, a legacy blob is upgraded to carry its key id.
	out, changed, err := plain.Rewrap(blob, []byte("id"))
	if err != nil || !changed {
		t.Fatalf("legacy rewrap: %v %v", err, changed)
	}
	if id, _ := KeyOf(out); id != plain.PrimaryID() {
		t.Error("upgraded blob should name its key")
	}
	if got, err := plain.Open(out, []byte("id")); err != nil || string(got) != "secret" {
		t.Fatalf("upgraded: %v", err)
	}
}

func TestRingValidation(t *testing.T) {
	k := key(t)
	if _, err := NewRing(k, k); err == nil {
		t.Error("the same key twice must be refused")
	}
	if _, err := NewRing(k, "not a key"); err == nil || !strings.Contains(err.Error(), "previous master key 1") {
		t.Errorf("a bad previous key should say which one: %v", err)
	}
	r, _ := NewRing(k)
	if strings.Contains(r.PrimaryID(), k[:8]) || len(r.PrimaryID()) != 8 {
		t.Errorf("key id: %s", r.PrimaryID())
	}
}
