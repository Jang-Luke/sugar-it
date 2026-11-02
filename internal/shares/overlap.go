package shares

import (
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"sugarintegration/internal/vfs"
)

// CheckOverlap returns share_overlap when candidatePath equals, contains or
// lies inside any existing share, after resolving symlinks. goos selects
// separator and case rules (windows and darwin compare case-insensitively),
// so foreign-OS paths can be checked on any host.
func CheckOverlap(existing []Spec, candidatePath, goos string) error {
	cand := normalize(candidatePath, goos)
	for _, s := range existing {
		o := normalize(s.Path, goos)
		if within(cand, o) || within(o, cand) {
			return vfs.E(vfs.CodeShareOverlap, "path overlaps share %q", s.Name)
		}
	}
	return nil
}

// normalize resolves symlinks where the path (or its nearest existing
// ancestor) exists on this host, falling back to lexical cleaning.
func normalize(p, goos string) string {
	win := goos == "windows"
	if win == (runtime.GOOS == "windows") {
		if abs, err := filepath.Abs(p); err == nil {
			p = resolve(abs)
		}
	}
	if win {
		p = strings.ReplaceAll(p, `\`, "/")
	} else {
		p = filepath.ToSlash(p)
	}
	p = path.Clean(p)
	if win || goos == "darwin" {
		p = strings.ToLower(p)
	}
	return p
}

// resolve evaluates symlinks of the longest existing prefix of p and
// re-appends the remainder.
func resolve(p string) string {
	var rest []string
	cur := filepath.Clean(p)
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// within reports whether a equals b or is below b (both slash-clean).
func within(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}
