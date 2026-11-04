package thread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketPath(t *testing.T) {
	path := func(gitDir string) string {
		p, err := SocketPath(gitDir)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := "/Users/someone/src/repo/.git"
	b := "/Users/someone/src/repo/.git/worktrees/feature"
	if path(a) != path(a) {
		t.Error("not stable for the same git dir")
	}
	if path(a) == path(b) {
		t.Error("worktrees share a socket")
	}
	// macOS limits sun_path to 104 bytes including the NUL, however deep the repo is.
	long := "/" + strings.Repeat("very-long-directory-name/", 20) + ".git"
	for _, dir := range []string{a, b, long} {
		if p := path(dir); len(p) >= 104 || !strings.HasSuffix(p, ".sock") {
			t.Errorf("SocketPath(%q) = %q (%d bytes)", dir, p, len(p))
		}
	}
}

func TestSocketDirPrivate(t *testing.T) {
	p, err := SocketPath("/repo/.git")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Dir(p))
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir %v, %v", fi.Mode(), err)
	}
}
