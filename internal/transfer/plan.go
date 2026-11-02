package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"slices"
	"strconv"
	"strings"

	"sugarintegration/internal/vfs"
)

// ResolveDst applies cp's destination rule: an existing directory or a path
// ending in "/" receives the source under its own name, the share root does
// too, and anything else is the new name itself.
func ResolveDst(ctx context.Context, src vfs.Entry, dst Dest, dstPath string) (string, error) {
	clean, err := vfs.Clean(dstPath)
	if err != nil {
		return "", err
	}
	if clean == "" || strings.HasSuffix(dstPath, "/") {
		return path.Join(clean, src.Name), nil
	}
	e, err := dst.Stat(ctx, clean)
	switch {
	case vfs.CodeOf(err) == vfs.CodeNotFound:
		return clean, nil
	case err != nil:
		return "", err
	case e.Type == vfs.TypeDir:
		return path.Join(clean, src.Name), nil
	}
	return clean, nil
}

// Validate rejects a spec that must not start: a malformed one, a copy or
// move onto itself or into itself on one peer and share, or a move out of a
// read-only share. Dst.Path is compared as given; the Manager validates again
// after ResolveDst, because "a" into the root resolves to "a". srcEntry is
// not consulted: a file is rejected the same way a directory is.
func Validate(s Spec, srcEntry vfs.Entry, srcReadOnly bool) error {
	return validate(s, srcReadOnly, func(p string) string { return p })
}

// validate compares paths through key, which says what is one file on the
// destination filesystem.
func validate(s Spec, srcReadOnly bool, key func(string) string) error {
	if s.Op != "copy" && s.Op != "move" {
		return vfs.E(vfs.CodeInvalidRequest, "unknown operation %q", s.Op)
	}
	switch s.OnConflict {
	case "", ConflictFail, ConflictSkip, ConflictOverwrite, ConflictRename:
	default:
		return vfs.E(vfs.CodeInvalidRequest, "unknown conflict policy %q", s.OnConflict)
	}
	from, err := vfs.Clean(s.Src.Path)
	if err != nil {
		return err
	}
	to, err := vfs.Clean(s.Dst.Path)
	if err != nil {
		return err
	}
	if s.Op == "move" && srcReadOnly {
		return vfs.E(vfs.CodeReadOnly, "cannot move out of a read-only share")
	}
	if s.Src.Peer == s.Dst.Peer && s.Src.Share == s.Dst.Share {
		from, to = key(from), key(to)
		if from == "" || to == from || strings.HasPrefix(to, from+"/") {
			return vfs.E(vfs.CodeInvalidRequest, "cannot %s %q onto or into itself", s.Op, s.Src.Path)
		}
	}
	return nil
}

// partKey names the part file of one item of one job.
func partKey(jobID, rel string) string {
	sum := sha256.Sum256([]byte(jobID + "\x00" + rel))
	return hex.EncodeToString(sum[:])[:32]
}

// plan is what one walk of the source found. Paths are relative to the root
// of the job's source ("" is the root itself); dirs lists parents first.
type plan struct {
	files    []planFile
	dirs     []planDir
	skipped  int
	failures []ItemError
}

type planFile struct {
	rel  string
	size int64
}

type planDir struct {
	rel   string
	empty bool
}

// buildPlan walks the source from its root entry. Symlinks and special files
// are counted and never followed, so links cannot leave the share or loop.
// Names come from the source, possibly a remote peer, so each must be one
// plain path component. An unreadable subdirectory fails on its own; the
// returned error means the root could not be read or the walk must stop.
func buildPlan(ctx context.Context, src vfs.FS, root string, e vfs.Entry) (*plan, error) {
	p := &plan{}
	switch e.Type {
	case vfs.TypeFile:
		p.files = append(p.files, planFile{"", e.Size})
	case vfs.TypeDir:
		if err := p.walk(ctx, src, root, ""); err != nil {
			return nil, err
		}
	default:
		p.skipped++
	}
	return p, nil
}

func (p *plan) walk(ctx context.Context, src vfs.FS, root, rel string) error {
	dir := path.Join(root, rel)
	es, err := src.List(ctx, dir)
	if err != nil {
		return err
	}
	p.dirs = append(p.dirs, planDir{rel, len(es) == 0})
	slices.SortFunc(es, func(a, b vfs.Entry) int { return strings.Compare(a.Name, b.Name) })
	for _, e := range es {
		if !plainName(e.Name) {
			p.failures = append(p.failures, ItemError{dir, vfs.CodeInvalidName,
				"source listed an unusable name " + strconv.Quote(e.Name)})
			continue
		}
		child := path.Join(rel, e.Name)
		switch e.Type {
		case vfs.TypeFile:
			p.files = append(p.files, planFile{child, e.Size})
		case vfs.TypeDir:
			err := p.walk(ctx, src, root, child)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return canceled()
			}
			if vfs.CodeOf(err) == vfs.CodePeerOffline {
				return err
			}
			p.failures = append(p.failures, ItemError{path.Join(root, child), vfs.CodeOf(err), err.Error()})
		default:
			p.skipped++
		}
	}
	return nil
}

func plainName(n string) bool {
	return n != "" && n != "." && n != ".." && !strings.ContainsAny(n, "/\x00")
}
