package request

import (
	"strings"
	"testing"

	"github.com/getplinth/plinth/internal/quota"
)

var opt = Options{Tiers: quota.Default(), DBTypes: []string{"postgresql", "mysql"}}

func mustParse(t *testing.T, s string) Request {
	t.Helper()
	r, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLegacyPayloadStringsAccepted(t *testing.T) {
	r := mustParse(t, `{"tenant":"team-a","projects":[
	  {"name":"sample-app","quota":"bronze","use_db":"true","db_type":"postgresql","application_port":"8080","host_url":"https://app.example.com"}]}`)
	if err := r.Validate(opt); err != nil {
		t.Fatal(err)
	}
	p := r.Projects[0]
	if !bool(p.UseDB) || p.Port != 8080 || p.Host() != "app.example.com" {
		t.Fatalf("%+v host=%s", p, p.Host())
	}
}

func TestDefaultsAndNormalization(t *testing.T) {
	r := mustParse(t, `{"tenant":"Team-A","projects":[{"name":" Data-Processor "}]}`)
	if err := r.Validate(opt); err != nil {
		t.Fatal(err)
	}
	if r.Projects[0].QuotaTier() != "bronze" || r.Projects[0].Normalized() != "data-processor" {
		t.Fatalf("%+v", r.Projects[0])
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]string{
		`{"tenant":"","projects":[{"name":"a"}]}`:                                                              "tenant is required",
		`{"tenant":"t","projects":[]}`:                                                                         "at least one project",
		`{"tenant":"t","projects":[{}]}`:                                                                       "missing a name",
		`{"tenant":"t","projects":[{"name":"!!!"}]}`:                                                           "invalid name",
		`{"tenant":"t","projects":[{"name":"a_b"}]}`:                                                           "DNS label",
		`{"tenant":"t","projects":[{"name":"a","quota":"platinum"}]}`:                                          "invalid quota tier",
		`{"tenant":"t","projects":[{"name":"a","use_db":true}]}`:                                               "no db_type",
		`{"tenant":"t","projects":[{"name":"a","use_db":true,"db_type":"oracle"}]}`:                            "unsupported db_type",
		`{"tenant":"t","projects":[{"name":"a"},{"name":"A "}]}`:                                               "duplicates",
		`{"tenant":"t","projects":[{"name":"a","host_url":"not a host"}]}`:                                     "invalid host_url",
		`{"tenant":"t","projects":[{"name":"a","application_port":70000}]}`:                                    "invalid application_port",
		`{"tenant":"` + strings.Repeat("x", 40) + `","projects":[{"name":"` + strings.Repeat("y", 30) + `"}]}`: "exceeds",
	}
	for in, want := range cases {
		err := mustParse(t, in).Validate(opt)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s\n  got %v, want substring %q", in, err, want)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Parse([]byte(`{"tenant":"t","projects":[],"surprise":1}`)); err == nil {
		t.Fatal("expected error for unknown field")
	}
}
