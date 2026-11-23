package syncer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// PathError is a bundle path that would escape or damage the repository.
type PathError struct {
	Path   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("refusing bundle path %q: %s", e.Path, e.Reason)
}

// ValidateRelPath checks the lexical form of a bundle path: relative,
// forward slashes, clean, no "..", and not inside .git or .orchestrai.
func ValidateRelPath(rel string) error {
	bad := func(reason string) error { return &PathError{Path: rel, Reason: reason} }
	switch {
	case rel == "":
		return bad("empty path")
	case strings.ContainsRune(rel, 0):
		return bad("contains NUL")
	case strings.Contains(rel, `\`):
		return bad("backslashes are not allowed; use forward slashes")
	case strings.HasPrefix(rel, "/") || filepath.IsAbs(rel):
		return bad("absolute paths are not allowed")
	}
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "":
			return bad("empty path segment")
		case ".", "..":
			return bad("relative segments (. or ..) are not allowed")
		}
	}
	if path.Clean(rel) != rel {
		return bad("path is not clean")
	}
	for _, seg := range strings.Split(rel, "/") {
		if unsafeSegmentChars(seg) {
			return bad("contains ':' or an invisible character")
		}
		if protectedSegment(seg) {
			return bad("writing inside " + seg + " is not allowed")
		}
	}
	return nil
}

// protectedSegment matches .git / .orchestrai in any position, including
// the forms other filesystems treat as equal: case and trailing dots/spaces
// (NTFS), 8.3 short names such as GIT~1 or GIT~4, alternate data streams
// (".git::$INDEX_ALLOCATION") and HFS+-ignorable code points.
func protectedSegment(seg string) bool {
	n := strings.ToLower(strings.TrimRight(seg, ". "))
	if n == ".git" || n == ".orchestrai" {
		return true
	}
	if i := strings.LastIndexByte(n, '~'); i > 0 && i < len(n)-1 && allDigits(n[i+1:]) {
		prefix := strings.TrimPrefix(n[:i], ".")
		if strings.HasPrefix("git", prefix) || strings.HasPrefix(prefix, "git") ||
			strings.HasPrefix("orchestrai", prefix) || strings.HasPrefix(prefix, "orches") {
			return true
		}
	}
	return false
}

// unsafeSegmentChars reports characters no managed path may contain: ':'
// (NTFS streams / drive letters) and code points HFS+ ignores in names.
func unsafeSegmentChars(seg string) bool {
	for _, r := range seg {
		switch {
		case r == ':':
			return true
		case r >= 0x200C && r <= 0x200F, r >= 0x202A && r <= 0x202E,
			r >= 0x206A && r <= 0x206F, r == 0xFEFF:
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Resolve maps a bundle path to the on-disk path inside root, refusing any
// symlinked component. realRoot must be the
// symlink-resolved root. The returned path may not exist yet.
func Resolve(realRoot, rel string) (string, error) {
	if err := ValidateRelPath(rel); err != nil {
		return "", err
	}
	segs := strings.Split(rel, "/")
	cur := realRoot
	for i, seg := range segs {
		next := filepath.Join(cur, seg)
		fi, err := os.Lstat(next)
		if errors.Is(err, fs.ErrNotExist) {
			// Nothing below here exists, so nothing can be a symlink.
			return filepath.Join(append([]string{cur}, segs[i:]...)...), nil
		}
		if err != nil {
			return "", err
		}
		last := i == len(segs)-1
		if fi.Mode()&fs.ModeSymlink != 0 {
			// Managed paths never follow symlinks: a committed link could
			// otherwise point a managed file at .git/hooks or outside the repo.
			return "", &PathError{Path: rel, Reason: "symlink at " + strings.Join(segs[:i+1], "/") + " (managed paths must not be symlinks)"}
		}
		if last {
			if fi.IsDir() {
				return "", &PathError{Path: rel, Reason: "is a directory"}
			}
			if !fi.Mode().IsRegular() {
				return "", &PathError{Path: rel, Reason: "is not a regular file"}
			}
			return next, nil
		}
		if !fi.IsDir() {
			return "", &PathError{Path: rel, Reason: strings.Join(segs[:i+1], "/") + " is not a directory"}
		}
		cur = next
	}
	return cur, nil
}

// RealRoot resolves symlinks in the repository root.
func RealRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
