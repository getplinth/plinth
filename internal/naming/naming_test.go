package naming

import "testing"

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"  My App  ":   "myapp",
		"Sample_App-1": "sample_app-1",
		"héllo wörld!": "hllowrld",
		"UPPER":        "upper",
		"!!!":          "",
		"a\tb\nc":      "abc",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q)=%q want %q", in, got, want)
		}
	}
}

func TestLegacyNames(t *testing.T) {
	if got := Namespace("team-a", "shop"); got != "team-a-shop" {
		t.Fatal(got)
	}
	if got := ResourceName("team-a-shop", SuffixRoleBinding); got != "team-a-shop-role-binding" {
		t.Fatal(got)
	}
	repo := RegistryRepo("team-a", "shop")
	if ScopeMap(repo) != "team-a-shop-scope-map" || RegistryToken(repo) != "team-a-shop-token" {
		t.Fatal("registry names")
	}
	if got := DBName("team-a", "shop", "DEV"); got != "team-a_shop_dev" {
		t.Fatal(got)
	}
	if got := CIVarDB("pass", "DEV"); got != "DB_PASS_DEV" {
		t.Fatal(got)
	}
	if got := RBACGroup("oidc:", "tenants", "team-a"); got != "oidc:tenants/team-a" {
		t.Fatal(got)
	}
}

func TestIsDNSLabel(t *testing.T) {
	ok := []string{"a", "team-a", "a1-b2"}
	bad := []string{"", "-a", "a-", "a_b", "A", "x" + string(make([]byte, 0)) + "yyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"}
	for _, s := range ok {
		if !IsDNSLabel(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range bad {
		if IsDNSLabel(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}
