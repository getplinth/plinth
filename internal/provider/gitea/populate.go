package gitea

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// templateURL turns a template reference into a clone URL and says whether
// the instance token may be sent to it. A reference is either "owner/name"
// on the same Gitea instance, or a full http(s) URL. The token is only ever
// sent to the Gitea host itself, never to a third-party template host.
func templateURL(base *url.URL, ref string) (cloneURL string, sendToken bool, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false, errors.New("gitea: empty template reference")
	}
	if !strings.Contains(ref, "://") {
		parts := strings.Split(ref, "/")
		if len(parts) != 2 || !nameRe.MatchString(parts[0]) || !nameRe.MatchString(parts[1]) {
			return "", false, fmt.Errorf("gitea: invalid template reference %q (want owner/name or an http(s) URL)", ref)
		}
		u := *base
		u.Path = strings.TrimRight(base.Path, "/") + "/" + ref + ".git"
		return u.String(), true, nil
	}
	u, err := url.Parse(ref)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", false, fmt.Errorf("gitea: template URL must be http(s): %q", ref)
	}
	if u.User != nil {
		return "", false, errors.New("gitea: template URL must not contain credentials")
	}
	return u.String(), u.Host == base.Host, nil
}

type gitRunner struct {
	home     string
	token    string
	caFile   string
	sendAuth bool
}

func (g *gitRunner) env(withAuth bool) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + g.home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Plinth", "GIT_AUTHOR_EMAIL=plinth@localhost",
		"GIT_COMMITTER_NAME=Plinth", "GIT_COMMITTER_EMAIL=plinth@localhost",
	}
	n := 0
	add := func(k, v string) {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, v))
		n++
	}
	// Credentials travel in an environment variable, not in a URL or argv,
	// so they never show up in process listings or git's own messages.
	if withAuth && g.token != "" {
		add("http.extraHeader", "Authorization: token "+g.token)
	}
	if g.caFile != "" {
		add("http.sslCAInfo", g.caFile)
	}
	add("protocol.allow", "never") // then allow only http(s) below
	add("protocol.https.allow", "always")
	add("protocol.http.allow", "always")
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", n))
	return env
}

func (g *gitRunner) run(ctx context.Context, dir string, withAuth bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env(withAuth)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if g.token != "" {
			msg = strings.ReplaceAll(msg, g.token, "***")
		}
		if len(msg) > 400 {
			msg = msg[:400]
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// populate copies a template repository (all branches and tags, with
// history) into an empty target repository, applying vars to text files on
// every branch. It returns the template's default branch.
func (d *Driver) populate(ctx context.Context, template, targetCloneURL string, vars map[string]string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("gitea: the git binary is required to populate repositories from a template")
	}
	srcURL, sendToken, err := templateURL(d.c.base, template)
	if err != nil {
		return "", err
	}

	tmp, err := os.MkdirTemp("", "plinth-populate-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	g := &gitRunner{home: tmp, token: d.c.token}
	if d.caPEM != "" {
		g.caFile = filepath.Join(tmp, "ca.pem")
		if err := os.WriteFile(g.caFile, []byte(d.caPEM), 0o600); err != nil {
			return "", err
		}
	}
	work := filepath.Join(tmp, "work")

	if _, err := g.run(ctx, tmp, sendToken, "clone", "--quiet", srcURL, work); err != nil {
		return "", fmt.Errorf("gitea: clone template: %w", err)
	}
	head, err := g.run(ctx, work, false, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("gitea: template has no default branch (is it empty?): %w", err)
	}
	head = strings.TrimPrefix(head, "origin/")

	refs, err := g.run(ctx, work, false, "for-each-ref", "--format=%(refname:strip=3)", "refs/remotes/origin")
	if err != nil {
		return "", err
	}
	for _, b := range strings.Split(refs, "\n") {
		if b == "" || b == "HEAD" {
			continue
		}
		if _, err := g.run(ctx, work, false, "checkout", "--quiet", "-B", b, "origin/"+b); err != nil {
			return "", err
		}
		if len(vars) > 0 {
			list, err := g.run(ctx, work, false, "ls-files", "-z")
			if err != nil {
				return "", err
			}
			changed, err := substitute(work, strings.FieldsFunc(list, func(r rune) bool { return r == 0 }), vars)
			if err != nil {
				return "", err
			}
			if len(changed) > 0 {
				if _, err := g.run(ctx, work, false, "add", "-u"); err != nil {
					return "", err
				}
				if _, err := g.run(ctx, work, false, "commit", "--quiet", "-m", "Initialize from template"); err != nil {
					return "", err
				}
			}
		}
	}

	if _, err := g.run(ctx, work, false, "remote", "set-url", "origin", targetCloneURL); err != nil {
		return "", err
	}
	if _, err := g.run(ctx, work, true, "push", "--quiet", "origin", "--all"); err != nil {
		return "", fmt.Errorf("gitea: push branches: %w", err)
	}
	if _, err := g.run(ctx, work, true, "push", "--quiet", "origin", "--tags"); err != nil {
		return "", fmt.Errorf("gitea: push tags: %w", err)
	}
	return head, nil
}
