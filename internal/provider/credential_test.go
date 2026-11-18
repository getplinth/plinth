package provider

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestCredentialIsRedacted(t *testing.T) {
	c := Credential{"password": "s3cret-value"}

	for name, out := range map[string]string{
		"%v":  fmt.Sprintf("%v", c),
		"%+v": fmt.Sprintf("%+v", c),
		"%s":  fmt.Sprintf("%s", c),
		"%#v": fmt.Sprintf("%#v", c),
	} {
		if strings.Contains(out, "s3cret") {
			t.Errorf("%s leaked the secret: %s", name, out)
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("issued", "cred", c)
	if strings.Contains(buf.String(), "s3cret") {
		t.Errorf("slog leaked the secret: %s", buf.String())
	}
}
