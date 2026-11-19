package auth

import (
	"strings"
	"testing"
)

func TestHashAndCheck(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") || strings.Contains(h, "correct") {
		t.Fatalf("unexpected encoding: %s", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("the right password must match")
	}
	for _, wrong := range []string{"", "correct horse batter", "Correct horse battery", "correct horse battery "} {
		if CheckPassword(h, wrong) {
			t.Errorf("%q must not match", wrong)
		}
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes of one password must differ (random salt)")
	}
}

func TestMalformedHashesNeverMatch(t *testing.T) {
	good, _ := HashPassword("whatever12345")
	parts := strings.Split(good, "$")
	for name, h := range map[string]string{
		"empty": "", "plain text": "whatever12345", "wrong algorithm": strings.Replace(good, "argon2id", "argon2i", 1),
		"huge memory":    strings.Replace(good, "m=65536", "m=999999999", 1),
		"zero passes":    strings.Replace(good, "t=3", "t=0", 1),
		"bad salt":       strings.Join(append(append([]string{}, parts[:4]...), "!!!", parts[5]), "$"),
		"truncated hash": strings.Join(append(append([]string{}, parts[:5]...), "AAAA"), "$"),
		"wrong version":  strings.Replace(good, "v=19", "v=16", 1),
	} {
		if CheckPassword(h, "whatever12345") {
			t.Errorf("%s: a malformed hash must not match", name)
		}
	}
}

func TestPasswordPolicy(t *testing.T) {
	for name, c := range map[string]struct {
		pw, user, email string
		ok              bool
	}{
		"good":               {"a long enough phrase", "alice", "alice@example.test", true},
		"short":              {"short1!", "alice", "", false},
		"eleven":             {"elevenchars", "x", "", false},
		"twelve":             {"twelvechars!", "x", "", true},
		"too long":           {strings.Repeat("a1", 65), "x", "", false},
		"contains username":  {"my-alice-secret-1", "alice", "", false},
		"contains mail name": {"xx-bobby-secret-1", "x", "bobby@example.test", false},
		"short username ok":  {"ab-secret-phrase", "ab", "", true},
		"all one character":  {"aaaaaaaaaaaaaa", "x", "", false},
		"common":             {"Password1234", "x", "", false},
		"unicode counts":     {"ääääöööö1234", "x", "", true},
	} {
		err := ValidatePassword(c.pw, c.user, c.email)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
		if err != nil {
			if _, isPolicy := err.(PolicyError); !isPolicy {
				t.Errorf("%s: want a PolicyError, got %T", name, err)
			}
		}
	}
}

func TestGeneratedPasswordsAreStrongAndPassThePolicy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword()
		if err != nil || len(p) != 20 || seen[p] {
			t.Fatalf("%q %v", p, err)
		}
		seen[p] = true
		if strings.ContainsAny(p, "0O1lI") {
			t.Errorf("look-alike characters in %q", p)
		}
		if err := ValidatePassword(p, "admin", ""); err != nil {
			t.Errorf("a generated password must pass the policy: %v", err)
		}
	}
}

func TestSessionTokens(t *testing.T) {
	a, _ := NewSessionToken()
	b, _ := NewSessionToken()
	if !strings.HasPrefix(a, SessionPrefix) || a == b || len(a) < 40 || strings.Contains(a, ".") {
		t.Errorf("%q %q", a, b)
	}
}

func TestUnknownUsersCostTheSameTime(t *testing.T) {
	SpendTime("anything") // must run without panicking and verify the dummy hash
}
