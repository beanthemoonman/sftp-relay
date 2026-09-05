package nas

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// ErrOutsideRoots is returned for any destination that does not resolve to a
// location inside one of the configured allowed roots.
var ErrOutsideRoots = errors.New("nas: destination is outside the allowed roots")

// ErrNoRoots is returned when no allowed roots are configured at all. Failing
// closed is deliberate: an empty allow-list must not mean "anywhere".
var ErrNoRoots = errors.New("nas: no allowed destination roots are configured")

// Resolver turns a path into its canonical form on the NAS, following symlinks.
// The live implementation is (*sftp.Client).RealPath.
type Resolver func(string) (string, error)

// CheckPath cleans p, resolves it with resolve (when non-nil) and confirms the
// result sits inside one of roots. It returns the cleaned, resolved path.
//
// Every handler that accepts a NAS path must go through this — a destination
// is never built by string concatenation elsewhere.
func CheckPath(p string, roots []string, resolve Resolver) (string, error) {
	clean, err := normalise(p)
	if err != nil {
		return "", err
	}

	cleanRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		cr, err := normalise(r)
		if err != nil {
			continue // a blank or relative root is ignored, not trusted
		}
		cleanRoots = append(cleanRoots, cr)
	}
	if len(cleanRoots) == 0 {
		return "", ErrNoRoots
	}

	// Check containment before resolving so an obviously bad path never reaches
	// the NAS, and again afterwards so a symlink cannot escape.
	if !contained(clean, cleanRoots) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoots, clean)
	}
	if resolve == nil {
		return clean, nil
	}

	resolved, err := resolve(clean)
	if err != nil {
		// An unresolvable path is usually one that does not exist yet (mkdir);
		// fall back to the nearest existing ancestor.
		if _, err := resolveAncestor(clean, cleanRoots, resolve); err != nil {
			return "", err
		}
		return clean, nil
	}
	rc, err := normalise(resolved)
	if err != nil {
		return "", fmt.Errorf("nas: resolved path %q is not usable: %w", resolved, err)
	}
	if !contained(rc, cleanRoots) {
		return "", fmt.Errorf("%w: %s resolves to %s", ErrOutsideRoots, clean, rc)
	}
	return rc, nil
}

// resolveAncestor walks up from p until a component resolves, and checks that
// the resolved ancestor is still inside an allowed root.
func resolveAncestor(p string, roots []string, resolve Resolver) (string, error) {
	for cur := path.Dir(p); ; cur = path.Dir(cur) {
		resolved, err := resolve(cur)
		if err == nil {
			rc, nerr := normalise(resolved)
			if nerr != nil {
				return "", fmt.Errorf("nas: resolved path %q is not usable: %w", resolved, nerr)
			}
			if !contained(rc, roots) {
				return "", fmt.Errorf("%w: %s resolves to %s", ErrOutsideRoots, cur, rc)
			}
			return rc, nil
		}
		if cur == "/" {
			return "", fmt.Errorf("nas: cannot resolve any ancestor of %s: %w", p, err)
		}
	}
}

// normalise NFC-normalises, requires an absolute path and cleans away `..`,
// duplicate separators and trailing slashes.
func normalise(p string) (string, error) {
	p = norm.NFC.String(strings.TrimSpace(p))
	if p == "" {
		return "", errors.New("nas: empty path")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("nas: path contains a null byte")
	}
	if !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("nas: path %q must be absolute", p)
	}
	return path.Clean(p), nil
}

// contained reports whether p is a root itself or a real child of one. A plain
// string prefix is not enough: /volume1/mediafoo is not inside /volume1/media.
func contained(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}
