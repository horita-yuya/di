package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

func TestSlug(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:owner/name.git\n":        "owner/name",
		"https://github.com/owner/name.git":      "owner/name",
		"https://github.com/owner/name":          "owner/name",
		"ssh://git@github.com:22/owner/name.git": "owner/name",
	} {
		if got := slug(url); got != want {
			t.Errorf("slug(%q) = %q, want %q", url, got, want)
		}
	}
}

// repo returns an empty repository on branch, isolated from the user's git config.
func repo(t *testing.T, branch string) (*Repo, func(args ...string)) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "test")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "test@example.com")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-b", branch)
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r, run
}

func write(t *testing.T, r *Repo, name, content string) {
	t.Helper()
	p := filepath.Join(r.Root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lines(prefix string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s %d\n", prefix, i+1)
	}
	return b.String()
}

func TestDiff(t *testing.T) {
	r, run := repo(t, "main")
	write(t, r, "keep.txt", lines("keep", 10))
	write(t, r, "old.txt", lines("moved", 10))
	write(t, r, "base.txt", "base\n")
	run("add", "-A")
	run("commit", "-m", "init")

	run("checkout", "-b", "feature")
	write(t, r, "keep.txt", strings.Replace(lines("keep", 10), "keep 2\n", "KEEP 2\n", 1))
	run("mv", "old.txt", "new.txt")
	write(t, r, ".gitignore", "ignored.txt\n")
	run("add", "-A")
	run("commit", "-m", "feature")

	// main moves on after the branch point; that must not show up as a change on feature.
	run("checkout", "main")
	write(t, r, "base.txt", "changed on main\n")
	run("commit", "-am", "main")
	run("checkout", "feature")

	// Uncommitted and untracked changes are part of the review.
	write(t, r, "keep.txt", strings.NewReplacer("keep 2\n", "KEEP 2\n", "keep 9\n", "KEEP 9\n").Replace(lines("keep", 10)))
	write(t, r, "sub/new.txt", "hello\n")
	write(t, r, "sub/with space.txt", "spaced\n")
	write(t, r, "メモ.txt", "memo\n")
	write(t, r, "ignored.txt", "secret\n")

	files, raw, err := r.Diff("main")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		var kind string
		switch {
		case f.IsRename:
			kind = "renamed from " + f.OldName
		case f.IsNew:
			kind = "new"
		default:
			var changed []string
			for _, frag := range f.TextFragments {
				for _, l := range frag.Lines {
					if l.Op != gitdiff.OpContext {
						changed = append(changed, l.Op.String()+strings.TrimSpace(l.Line))
					}
				}
			}
			kind = strings.Join(changed, ",")
		}
		got[f.NewName] = kind
	}
	want := map[string]string{
		".gitignore":         "new",
		"keep.txt":           "-keep 2,+KEEP 2,-keep 9,+KEEP 9",
		"new.txt":            "renamed from old.txt",
		"sub/new.txt":        "new",
		"sub/with space.txt": "new",
		"メモ.txt":             "new",
	}
	if len(got) != len(want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	for name, kind := range want {
		if got[name] != kind {
			t.Errorf("%s: %q, want %q", name, got[name], kind)
		}
	}
	for _, f := range files {
		if name := f.NewName; name == "sub/new.txt" && (len(f.TextFragments) != 1 || f.TextFragments[0].Lines[0].Line != "hello\n") {
			t.Errorf("untracked content not parsed: %+v", f.TextFragments)
		}
	}

	// raw changes exactly when the working tree does (hot reload).
	_, again, err := r.Diff("main")
	if err != nil || again != raw {
		t.Fatalf("raw diff not stable: %v", err)
	}
	write(t, r, "sub/new.txt", "hello again\n")
	if _, changed, _ := r.Diff("main"); changed == raw {
		t.Fatal("raw diff did not change with an untracked file")
	}

	if _, _, err := r.Diff("no-such-branch"); err == nil {
		t.Fatal("want an error for an unknown base")
	}
}

func TestOpen(t *testing.T) {
	r, run := repo(t, "main")
	write(t, r, "sub/a.txt", "a\n")
	run("add", "-A")
	run("commit", "-m", "init")

	from, err := Open(filepath.Join(r.Root, "sub"))
	if err != nil || *from != *r {
		t.Fatalf("Open(sub) = %+v, %v; want %+v", from, err, r)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-b", "other", wt)
	w, err := Open(wt)
	if err != nil {
		t.Fatal(err)
	}
	if w.GitDir == r.GitDir || w.Root == r.Root {
		t.Fatalf("worktree shares Root/GitDir with the main checkout: %+v %+v", w, r)
	}
	if w.Branch() != "other" || r.Branch() != "main" {
		t.Fatalf("Branch = %q, %q", w.Branch(), r.Branch())
	}
}

func TestRepoSlug(t *testing.T) {
	r, run := repo(t, "main")
	if got := r.Slug(); got != "" {
		t.Fatalf("Slug without origin = %q", got)
	}
	for _, url := range []string{"git@github.com:owner/name.git", "https://github.com/owner/name.git", "https://github.com/owner/name"} {
		run("remote", "add", "origin", url)
		if got := r.Slug(); got != "owner/name" {
			t.Errorf("Slug with origin %s = %q", url, got)
		}
		run("remote", "remove", "origin")
	}
}

func TestDefaultBase(t *testing.T) {
	t.Run("origin/HEAD", func(t *testing.T) {
		src, run := repo(t, "trunk")
		write(t, src, "a.txt", "a\n")
		run("add", "-A")
		run("commit", "-m", "init")
		dst := filepath.Join(t.TempDir(), "clone")
		run("clone", src.Root, dst)
		r, err := Open(dst)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.DefaultBase(); got != "origin/trunk" {
			t.Fatalf("DefaultBase = %q", got)
		}
	})
	for branch, want := range map[string]string{"main": "main", "master": "master"} {
		t.Run(branch, func(t *testing.T) {
			r, run := repo(t, branch)
			write(t, r, "a.txt", "a\n")
			run("add", "-A")
			run("commit", "-m", "init")
			run("checkout", "-b", "feature")
			if got := r.DefaultBase(); got != want {
				t.Fatalf("DefaultBase = %q, want %q", got, want)
			}
		})
	}
}
