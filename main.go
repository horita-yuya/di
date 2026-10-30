// Command di reviews the current branch's diff in the terminal and sends comments to Claude Code.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/horitayuya/di/internal/config"
	"github.com/horitayuya/di/internal/git"
	"github.com/horitayuya/di/internal/mcp"
	"github.com/horitayuya/di/internal/thread"
	"github.com/horitayuya/di/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "di:", err)
		os.Exit(1)
	}
}

func run() error {
	repo, err := git.Open(".")
	if err != nil {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		return mcp.Serve(context.Background(), thread.SocketPath(repo.GitDir), repo.Slug())
	}

	baseFlag := flag.String("base", "", "base ref to diff against (default: config, then origin/HEAD, main, master)")
	flag.Parse()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slug := repo.Slug()
	rc := cfg.Repo(slug)
	base := cmp.Or(*baseFlag, rc.Base)
	if base == "" {
		base = repo.DefaultBase()
	}
	return tui.Run(repo, slug, rc, base)
}
