# di

GitHub's "Files changed" in your terminal. Review the diff between your working branch and the base branch,
and send line comments to Claude via MCP (Claude Code channels) so it can address them right away.

## Install

Download an archive for your platform from [Releases](https://github.com/horita-yuya/di/releases) and put `di` on your PATH, or:

```sh
go install github.com/horita-yuya/di@latest
GOBIN=~/.local/bin go install .   # from a clone, into any directory on your PATH
```

`di upgrade` updates a release binary in place to the latest release (the archive is verified against the release's SHA-256 checksums).
Every push to main is released automatically as the next patch version.

## Usage

```sh
di [--base <ref>]   # start the TUI (base: --base > config > origin/HEAD > main > master)
di mcp              # stdio MCP server (started by Claude Code)
di upgrade          # replace this binary with the latest release
```

The diff runs from the merge-base with base to the working tree (including uncommitted and untracked files) and refreshes every second.
Comment threads live only inside the di process and are gone when di exits; after a restart, the `Restore` button in the empty AI Replies panel asks Claude to send them back from its conversation. One di serves every worktree of the repository it was started in (each worktree's Claude still connects to that worktree's socket): with more than one, a Worktrees section at the top of the left pane lists them with their branch, a `●` when Claude is connected and `●N` for unanswered Claude replies, and clicking one shows its diff, threads and connection. Worktrees are listed at startup, so restart di after adding one; a worktree already served by another di is left to it.

Light theme only. While running, di switches the terminal background to white (OSC 10/11) and restores it on exit (OSC 110/111).

The footer has base buttons for the configured base, `origin/main` and `HEAD` (uncommitted changes only: staged, unstaged and untracked). Click one to choose the diff target.
The footer's `Commit & Push` button opens the input for a commit message; `enter` or `Commit` runs `git add -A`, `git commit` and `git push -u origin HEAD`.
The `Open PR` button opens the branch's pull request in the browser via `gh` (or the create page if none exists).
An empty message uses the Unix timestamp. Errors are shown in the footer.

### Controls

Mouse driven (the keyboard only does `↑` / `↓` and `ctrl+c` to quit).

| Action | Effect |
| --- | --- |
| Click a worktree (top of the left pane) | Show that worktree's diff, threads and Claude connection |
| Wheel | Scroll the pane under the pointer (tree / diff) |
| Click in the tree | Open / close a directory, or jump to a file (its arrow folds / unfolds the file) |
| `↑` / `↓` | Jump to the previous / next file in the tree |
| Click a file header | Fold / unfold the file |
| Click a code line | Comment on that line (while typing, switches the target line and keeps the draft) |
| `Comment` at the end of a thread | Add a comment to the thread |
| `Resolve` in AI Replies (under the tree) | Delete the thread and tell Claude; click the rest of the entry to jump to it |
| `Comment` / `Cancel` under the input (opens right below the line, aligned with the code text) | Send / discard (`enter` also sends; newline with `alt+enter` or `ctrl+j`) |
| Base button in the footer | Diff against that base |
| `Commit & Push` in the footer | Commit all changes and push |
| `Open PR` in the footer | Open the branch's pull request (or the create page) in the browser |
| `Restore` in AI Replies (shown when it is empty) | Ask Claude to restore the unresolved threads after di restarted (the note after it shows the request, then how many threads came back) |

Thread comments wrap to the width of the diff pane. Files with an unanswered Claude reply (and closed folders containing them) are highlighted in the tree, and listed in the AI Replies panel below it (in memory only), which is always shown and holds the `Restore` button while empty.

## Config

`$XDG_CONFIG_HOME/di/config.yaml` (`~/.config/di/config.yaml` if unset)

```yaml
repos:
  - repo: owner/name        # owner/name of the origin remote
    base: origin/develop    # optional
    collapsed:              # files folded by default (doublestar patterns, relative to the repo root)
      - "**/*.pb.go"
      - "pnpm-lock.yaml"
      - "vendor/**"
```

## Claude Code setup

```sh
claude mcp add --scope user di -- ~/.local/bin/di mcp
claude --dangerously-load-development-channels server:di
```

The 2026-07-28 protocol revision does not register the channel, so `di mcp` only accepts earlier revisions.

Comments you send from di reach Claude as channel notifications, and AI Replies with `reply_comment`.
While Claude works on a thread it reports progress with `set_status`, shown in the thread header as `💬 [AI: editing…]` until it replies.
Resolving a thread tells Claude it is done; `Restore` asks Claude to push the threads it remembers back with `restore_comments`.
Ask Claude something like "fold the generated code" and it updates `collapsed` in the config via `get_collapsed` / `set_collapsed`, applied immediately to the running di
(the config is rewritten, so YAML comments are lost; this is unavailable in repositories without an origin remote).

Without channels, ask Claude to "handle the di comments" and it fetches them with `list_comments`.
