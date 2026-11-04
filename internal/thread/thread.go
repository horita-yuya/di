// Package thread defines review threads and the TUI ⇄ `di mcp` socket protocol.
// Threads live only in the TUI process.
package thread

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
)

type Message struct {
	Author string `json:"author" jsonschema:"user or claude"`
	Body   string `json:"body"`
}

type Thread struct {
	ID       string    `json:"id" jsonschema:"the thread's original id"`
	File     string    `json:"file"`
	Side     string    `json:"side" jsonschema:"new or old"`
	Line     int       `json:"line" jsonschema:"line number on that side"`
	LineText string    `json:"line_text" jsonschema:"content of that line, without the diff prefix"`
	Context  string    `json:"context" jsonschema:"diff excerpt around the line, each line prefixed with a space, + or -"`
	Messages []Message `json:"messages" jsonschema:"the full history, in order"`
}

// ToMCP is sent TUI → mcp.
type ToMCP struct {
	Thread   *Thread `json:"thread,omitempty"`   // the user added a message
	Resolved string  `json:"resolved,omitempty"` // thread ID the user resolved
	Restore  bool    `json:"restore,omitempty"`  // the user asked Claude to restore threads
}

// ToTUI is sent mcp → TUI.
type ToTUI struct {
	Reply   *Reply   `json:"reply,omitempty"`
	Restore []Thread `json:"restore"` // no omitempty: an empty restore_comments must still reach the TUI, as a non-nil slice
	Status  *Status  `json:"status,omitempty"`
}

// Reply is Claude's answer to a thread.
type Reply struct {
	ID   string `json:"id" jsonschema:"thread id"`
	Body string `json:"body" jsonschema:"reply shown to the user in di"`
}

// Status is Claude's progress note on a thread it is working on.
type Status struct {
	ID   string `json:"id" jsonschema:"thread id"`
	Text string `json:"text" jsonschema:"a few words on what you are doing now, e.g. \"reading tui.go\", \"editing\", \"running tests\""`
}

// SocketPath is unique per worktree. It is under /tmp rather than $TMPDIR, which can differ between
// the shell running di and Claude Code spawning `di mcp`, and stays within the sun_path length limit.
// It sits in a per-user 0700 directory so other local users can neither squat it nor connect to it.
func SocketPath(gitDir string) (string, error) {
	dir := fmt.Sprintf("/tmp/di-%d", os.Getuid())
	os.Mkdir(dir, 0o700)
	// Only the owner can chmod, so this fails when another user created dir first.
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() || os.Chmod(dir, 0o700) != nil {
		return "", fmt.Errorf("%s is not a directory you own; remove it", dir)
	}
	sum := sha256.Sum256([]byte(gitDir))
	return fmt.Sprintf("%s/%s.sock", dir, hex.EncodeToString(sum[:])[:12]), nil
}
