package gitea

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxSubstFileSize = 1 << 20 // files larger than 1 MiB are left alone

// substitute replaces every key of vars by its value in the given files
// (paths relative to dir). Binary files, symlinks and large files are
// skipped. It returns the files that changed. Replacement is literal; no
// regular expressions or shell are involved.
func substitute(dir string, files []string, vars map[string]string) ([]string, error) {
	if len(vars) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		if k == "" {
			return nil, fmt.Errorf("gitea: empty template variable name")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic
	pairs := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		pairs = append(pairs, k, vars[k])
	}
	r := strings.NewReplacer(pairs...)

	var changed []string
	for _, rel := range files {
		p := filepath.Join(dir, rel)
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSubstFileSize {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		head := b
		if len(head) > 8000 {
			head = head[:8000]
		}
		if bytes.IndexByte(head, 0) >= 0 { // binary
			continue
		}
		out := r.Replace(string(b))
		if out == string(b) {
			continue
		}
		if err := os.WriteFile(p, []byte(out), fi.Mode().Perm()); err != nil {
			return nil, err
		}
		changed = append(changed, rel)
	}
	return changed, nil
}
