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
	Author string `json:"author"` // "user" | "claude"
	Body   string `json:"body"`
}

// Thread is sent TUI → mcp whenever the user adds a message.
type Thread struct {
	ID       string    `json:"id"`
	File     string    `json:"file"`
	Side     string    `json:"side"` // "new" | "old"
	Line     int       `json:"line"`
	LineText string    `json:"line_text"`
	Context  string    `json:"context"`
	Messages []Message `json:"messages"`
}

// Reply is sent mcp → TUI when Claude answers a thread.
type Reply struct {
	ID   string `json:"id" jsonschema:"thread id"`
	Body string `json:"body" jsonschema:"reply shown to the user in di"`
}

// SocketPath is unique per worktree. It is under /tmp rather than $TMPDIR, which can differ between
// the shell running di and Claude Code spawning `di mcp`, and stays within the sun_path length limit.
func SocketPath(gitDir string) string {
	sum := sha256.Sum256([]byte(gitDir))
	return fmt.Sprintf("/tmp/di-%d-%s.sock", os.Getuid(), hex.EncodeToString(sum[:])[:12])
}
