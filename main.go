// Command di reviews the current branch's diff in the terminal and sends comments to Claude Code.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/horita-yuya/di/internal/config"
	"github.com/horita-yuya/di/internal/git"
	"github.com/horita-yuya/di/internal/mcp"
	"github.com/horita-yuya/di/internal/thread"
	"github.com/horita-yuya/di/internal/tui"
	"github.com/horita-yuya/di/internal/upgrade"
)

var version = "dev" // set by goreleaser

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "di:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		return upgrade.Run(version)
	}
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
