package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Passwords are stored as argon2id hashes in the standard encoded form
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash). The parameters follow the
// OWASP recommendation (64 MiB, 3 passes, 2 lanes); a hash records its own, so
// they can be raised later without invalidating anything. Hashing uses memory,
// so only a few run at once: a flood of sign-in attempts queues instead of
// exhausting the server.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16

	// MinPasswordLength and MaxPasswordLength bound what people may choose.
	MinPasswordLength = 12
	MaxPasswordLength = 128
)

var hashSlots = make(chan struct{}, 4)

func acquire() func() {
	hashSlots <- struct{}{}
	return func() { <-hashSlots }
}

// HashPassword returns the encoded argon2id hash of a password.
func HashPassword(password string) (string, error) {
	defer acquire()()
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches an encoded hash, comparing in
// constant time. A malformed hash never matches.
func CheckPassword(encoded, password string) bool {
	var version, m, t, p int
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	// The hash comes from our own database, but bound the work anyway.
	if m < 8 || m > 256*1024 || t < 1 || t > 10 || p < 1 || p > 16 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) < 16 || len(want) > 64 || len(salt) < 8 {
		return false
	}
	defer acquire()()
	got := argon2.IDKey([]byte(password), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash lets a sign-in for an unknown username take as long as one for a
// real account, so response time does not reveal which usernames exist.
var dummyHash = func() string {
	h, err := HashPassword("plinth-timing-equaliser")
	if err != nil {
		panic(err)
	}
	return h
}()

// SpendTime does the work of checking a password without any account behind it.
func SpendTime(password string) { _ = CheckPassword(dummyHash, password) }

// PolicyError explains why a password was refused, in a sentence for the person who chose it.
type PolicyError string

func (e PolicyError) Error() string { return string(e) }

var common = map[string]bool{
	"password1234": true, "passwordpassword": true, "123456789012": true, "qwertyuiop12": true, "administrator": true,
	"letmein12345": true, "welcome12345": true, "changeme1234": true, "plinthplinth": true, "iloveyou1234": true,
}

// ValidatePassword checks a new password. username and email are the account's,
// so a password cannot simply be the name.
func ValidatePassword(password, username, email string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordLength:
		return PolicyError(fmt.Sprintf("the password must be at least %d characters", MinPasswordLength))
	case len(password) > MaxPasswordLength:
		return PolicyError(fmt.Sprintf("the password can be at most %d characters", MaxPasswordLength))
	}
	low := strings.ToLower(password)
	for _, name := range []string{username, strings.SplitN(email, "@", 2)[0]} {
		if len(name) >= 3 && strings.Contains(low, strings.ToLower(name)) {
			return PolicyError("the password must not contain the username or e-mail name")
		}
	}
	if common[low] || strings.Trim(low, string([]rune(low)[:1])) == "" {
		return PolicyError("that password is too easy to guess")
	}
	return nil
}

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no look-alikes

// GeneratePassword returns a random password of 20 characters (about 115 bits).
func GeneratePassword() (string, error) {
	out := make([]byte, 20)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// SessionPrefix starts every session token, which is how they are told apart
// from identity provider tokens (JWTs, which contain dots and never start so).
const SessionPrefix = "plt_"

// NewSessionToken returns a random session token: 256 bits, URL-safe.
func NewSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return SessionPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ErrNoSession reports a bearer token that is not a session token.
var ErrNoSession = errors.New("not a session token")
