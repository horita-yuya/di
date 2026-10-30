// Package git wraps the git commands di needs.
package git

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

type Repo struct {
	Root   string
	GitDir string // per worktree
}

func Open(dir string) (*Repo, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	root, gitDir, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return &Repo{Root: root, GitDir: gitDir}, nil
}

// Slug returns "owner/name" of the origin remote, or "" if there is none.
func (r *Repo) Slug() string {
	url, err := git(r.Root, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return slug(url)
}

func slug(url string) string {
	parts := strings.Split(strings.ReplaceAll(strings.TrimSuffix(strings.TrimSpace(url), ".git"), ":", "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

func (r *Repo) Branch() string {
	out, _ := git(r.Root, "branch", "--show-current")
	return strings.TrimSpace(out)
}

func (r *Repo) DefaultBase() string {
	if out, err := git(r.Root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	if _, err := git(r.Root, "rev-parse", "--verify", "--quiet", "main"); err != nil {
		return "master"
	}
	return "main"
}

// Diff returns the changes from the merge-base with base to the working tree,
// including untracked files, along with the raw diff text.
func (r *Repo) Diff(base string) ([]*gitdiff.File, string, error) {
	raw, err := git(r.Root, "diff", "--merge-base", base, "--no-color", "--no-ext-diff", "-M")
	if err != nil {
		return nil, "", err
	}
	untracked, err := git(r.Root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, "", err
	}
	for f := range strings.SplitSeq(strings.TrimSuffix(untracked, "\x00"), "\x00") {
		if f == "" {
			continue
		}
		out, err := git(r.Root, "diff", "--no-index", "--no-color", "--no-ext-diff", "--", "/dev/null", f)
		// --no-index exits 1 when the files differ.
		if ee, ok := errors.AsType[*exec.ExitError](err); err != nil && !(ok && ee.ExitCode() == 1) {
			return nil, "", err
		}
		raw += out
	}
	files, _, err := gitdiff.Parse(strings.NewReader(raw))
	return files, raw, err
}

// CommitPush stages everything, commits it with msg and pushes the current branch to origin.
func (r *Repo) CommitPush(msg string) error {
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", msg}, {"push", "-u", "origin", "HEAD"}} {
		if _, err := git(r.Root, args...); err != nil {
			return err
		}
	}
	return nil
}

// OpenPR opens the current branch's pull request in the browser, or the page to create one.
func (r *Repo) OpenPR() error {
	var err error
	for _, sub := range []string{"view", "create"} {
		cmd := exec.Command("gh", "pr", sub, "--web")
		cmd.Dir = r.Root
		if _, err = cmd.Output(); err == nil {
			return nil
		}
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Errorf("gh pr create --web: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
	}
	return string(out), err
}
