package nas

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var roots = []string{"/volume1/media", "/volume2/backups/"}

// fakeResolver stands in for (*sftp.Client).RealPath. Anything in links is a
// symlink; anything in missing does not exist yet.
func fakeResolver(links map[string]string, missing ...string) Resolver {
	return func(p string) (string, error) {
		for _, m := range missing {
			if p == m {
				return "", fmt.Errorf("no such file: %s", p)
			}
		}
		if target, ok := links[p]; ok {
			return target, nil
		}
		return p, nil
	}
}

func TestCheckPathWithoutResolver(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		roots   []string
		want    string
		wantErr error
	}{
		{name: "root itself", path: "/volume1/media", roots: roots, want: "/volume1/media"},
		{name: "child", path: "/volume1/media/films", roots: roots, want: "/volume1/media/films"},
		{name: "deep child", path: "/volume1/media/a/b/c", roots: roots, want: "/volume1/media/a/b/c"},
		{name: "second root", path: "/volume2/backups/x", roots: roots, want: "/volume2/backups/x"},
		{name: "trailing slash", path: "/volume1/media/films/", roots: roots, want: "/volume1/media/films"},
		{name: "duplicate separators", path: "/volume1//media///films", roots: roots, want: "/volume1/media/films"},
		{name: "dot segments", path: "/volume1/media/./films", roots: roots, want: "/volume1/media/films"},
		{name: "surrounding space", path: "  /volume1/media  ", roots: roots, want: "/volume1/media"},
		{name: "root with trailing slash configured", path: "/volume2/backups", roots: roots, want: "/volume2/backups"},
		{name: "unicode NFD normalises to NFC", path: "/volume1/media/café", roots: roots, want: "/volume1/media/café"},

		{name: "traversal out", path: "/volume1/media/../../etc", roots: roots, wantErr: ErrOutsideRoots},
		{name: "traversal to sibling", path: "/volume1/media/../mediafoo", roots: roots, wantErr: ErrOutsideRoots},
		{name: "prefix match but not a child", path: "/volume1/mediafoo", roots: roots, wantErr: ErrOutsideRoots},
		{name: "prefix match deeper", path: "/volume1/mediafoo/bar", roots: roots, wantErr: ErrOutsideRoots},
		{name: "unrelated absolute path", path: "/etc/passwd", roots: roots, wantErr: ErrOutsideRoots},
		{name: "filesystem root", path: "/", roots: roots, wantErr: ErrOutsideRoots},
		{name: "parent of a root", path: "/volume1", roots: roots, wantErr: ErrOutsideRoots},

		{name: "no roots configured", path: "/volume1/media", roots: nil, wantErr: ErrNoRoots},
		{name: "only blank roots configured", path: "/volume1/media", roots: []string{"", "  "}, wantErr: ErrNoRoots},
		{name: "relative roots are ignored", path: "/volume1/media", roots: []string{"volume1/media"}, wantErr: ErrNoRoots},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckPath(tt.path, tt.roots, nil)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CheckPath(%q) error = %v, want %v", tt.path, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckPath(%q): %v", tt.path, err)
			}
			if got != tt.want {
				t.Errorf("CheckPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestCheckPathRejectsMalformed(t *testing.T) {
	for _, p := range []string{"", "   ", "relative/path", "volume1/media", "/volume1/media/\x00evil"} {
		t.Run(fmt.Sprintf("%q", p), func(t *testing.T) {
			if _, err := CheckPath(p, roots, nil); err == nil {
				t.Errorf("CheckPath(%q) succeeded, want an error", p)
			}
		})
	}
}

func TestCheckPathFollowsSymlinks(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		links   map[string]string
		missing []string
		want    string
		wantErr error
	}{
		{
			name:  "symlink staying inside a root",
			path:  "/volume1/media/link",
			links: map[string]string{"/volume1/media/link": "/volume1/media/real"},
			want:  "/volume1/media/real",
		},
		{
			name:  "symlink pointing at another allowed root",
			path:  "/volume1/media/link",
			links: map[string]string{"/volume1/media/link": "/volume2/backups/real"},
			want:  "/volume2/backups/real",
		},
		{
			name:    "symlink escaping to /etc",
			path:    "/volume1/media/link",
			links:   map[string]string{"/volume1/media/link": "/etc"},
			wantErr: ErrOutsideRoots,
		},
		{
			name:    "symlink escaping to a prefix-matching sibling",
			path:    "/volume1/media/link",
			links:   map[string]string{"/volume1/media/link": "/volume1/mediafoo"},
			wantErr: ErrOutsideRoots,
		},
		{
			name:    "missing path falls back to its parent, which is allowed",
			path:    "/volume1/media/new-folder",
			missing: []string{"/volume1/media/new-folder"},
			want:    "/volume1/media/new-folder",
		},
		{
			name:    "missing path whose parent symlinks outside",
			path:    "/volume1/media/link/new-folder",
			links:   map[string]string{"/volume1/media/link": "/etc"},
			missing: []string{"/volume1/media/link/new-folder"},
			wantErr: ErrOutsideRoots,
		},
		{
			name:    "nothing at all resolves",
			path:    "/volume1/media/a/b",
			missing: []string{"/volume1/media/a/b", "/volume1/media/a", "/volume1/media", "/volume1", "/"},
			wantErr: nil, // asserted as a plain error below
		},
		{
			name:    "resolver returns a relative path",
			path:    "/volume1/media/link",
			links:   map[string]string{"/volume1/media/link": "not-absolute"},
			wantErr: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckPath(tt.path, roots, fakeResolver(tt.links, tt.missing...))
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
			case tt.want == "":
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
			default:
				if err != nil {
					t.Fatalf("CheckPath: %v", err)
				}
				if got != tt.want {
					t.Errorf("got %q, want %q", got, tt.want)
				}
			}
		})
	}
}

func TestSplitRoots(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"/a", []string{"/a"}},
		{"/a,/b", []string{"/a", "/b"}},
		{" /a , /b ", []string{"/a", "/b"}},
		{"/a\n/b\n", []string{"/a", "/b"}},
		{"/a,,\n,/b", []string{"/a", "/b"}},
	}
	for _, tt := range tests {
		got := SplitRoots(tt.in)
		if strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("SplitRoots(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
