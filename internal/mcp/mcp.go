// Package mcp relays review threads between the di TUI (unix socket) and Claude Code (stdio MCP).
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/horita-yuya/di/internal/config"
	"github.com/horita-yuya/di/internal/thread"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Messages arriving as <channel source="di"> are review comment threads the user wrote in di (a terminal diff reviewer). ` +
	`After handling one (fixing code or answering a question), always answer with reply_comment. ` +
	`Ask the user questions or confirmations via reply_comment as well; they read and reply in di.`

// transport captures the stdio connection so the relay can send notifications go-sdk does not know about.
type transport struct{ onConnect func(mcp.Connection) }

func (t *transport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := (&mcp.StdioTransport{}).Connect(ctx)
	if err == nil {
		t.onConnect(conn)
	}
	return conn, err
}

type relay struct {
	mu      sync.Mutex
	tui     net.Conn        // nil while the TUI is not running
	threads []thread.Thread // received from the TUI, for list_comments
}

func Serve(ctx context.Context, sock, slug string) error {
	r := &relay{}
	s := mcp.NewServer(&mcp.Implementation{Name: "di", Version: "0.1.0"}, &mcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{Experimental: map[string]any{"claude/channel": map[string]any{}}},
		// Claude Code does not register channels on the 2026-07-28 revision, so negotiate an older one.
		SupportedProtocolVersions: slices.DeleteFunc(mcp.SupportedProtocolVersions(), func(v string) bool { return v >= "2026-07-28" }),
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_comments",
		Description: "List di review comment threads.",
	}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var texts []string
		for _, t := range r.threads {
			texts = append(texts, format(t))
		}
		text := strings.Join(texts, "\n\n---\n\n")
		if text == "" {
			text = "No comments."
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "reply_comment",
		Description: "Reply to a di review comment thread. The user reads it in di.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in thread.Reply) (*mcp.CallToolResult, any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.tui == nil {
			return nil, nil, errors.New("di is not running")
		}
		if err := json.NewEncoder(r.tui).Encode(thread.ToTUI{Reply: &in}); err != nil {
			return nil, nil, err
		}
		if i := slices.IndexFunc(r.threads, func(t thread.Thread) bool { return t.ID == in.ID }); i >= 0 {
			r.threads[i].Messages = append(r.threads[i].Messages, thread.Message{Author: "claude", Body: in.Body})
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent"}}}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "restore_comments",
		Description: "Restore di review threads into di after it restarted, when di asks for it.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Threads []thread.Thread `json:"threads"`
	}) (*mcp.CallToolResult, any, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.tui == nil {
			return nil, nil, errors.New("di is not running")
		}
		if err := json.NewEncoder(r.tui).Encode(thread.ToTUI{Restore: in.Threads}); err != nil {
			return nil, nil, err
		}
		for _, t := range in.Threads {
			r.upsert(t)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("restored %d", len(in.Threads))}}}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_collapsed",
		Description: "Get the glob patterns of paths di shows collapsed by default in this repository (file tree and diff).",
	}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		cfg, err := config.Load()
		if err != nil {
			return nil, nil, err
		}
		text := strings.Join(cfg.Repo(slug).CollapsedGlobs, "\n")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: cmp.Or(text, "(none)")}}}, nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "set_collapsed",
		Description: "Replace the glob patterns (doublestar, relative to the repository root, e.g. \"**/*.pb.go\", \"vendor/**\") " +
			"of paths di shows collapsed by default in this repository. Call get_collapsed first to keep existing ones. A running di applies them immediately.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in struct {
		Patterns []string `json:"patterns"`
	}) (*mcp.CallToolResult, any, error) {
		if slug == "" {
			return nil, nil, errors.New("this repository has no origin remote; di config is keyed by owner/name")
		}
		cfg, err := config.Load()
		if err == nil {
			err = cfg.SetCollapsed(slug, in.Patterns)
		}
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "saved"}}}, nil, nil
	})

	return s.Run(ctx, &transport{onConnect: func(c mcp.Connection) { go r.run(ctx, sock, c) }})
}

// run keeps (re)connecting to the TUI and pushes everything it sends to Claude as channel notifications.
func (r *relay) run(ctx context.Context, sock string, out mcp.Connection) {
	for ; ; time.Sleep(time.Second) {
		c, err := net.Dial("unix", sock)
		if err != nil {
			continue
		}
		r.mu.Lock()
		r.tui = c
		r.mu.Unlock()

		for dec := json.NewDecoder(c); ; {
			var in thread.ToMCP
			if err := dec.Decode(&in); err != nil {
				break
			}
			var content string
			var meta map[string]string
			r.mu.Lock()
			switch {
			case in.Thread != nil:
				t := *in.Thread
				r.upsert(t)
				content, meta = format(t), map[string]string{"thread_id": t.ID, "file": t.File, "line": fmt.Sprint(t.Line)}
			case in.Resolved != "":
				r.threads = slices.DeleteFunc(r.threads, func(t thread.Thread) bool { return t.ID == in.Resolved })
				content = fmt.Sprintf("The user resolved di review thread %s. Nothing more is needed for it.", in.Resolved)
				meta = map[string]string{"thread_id": in.Resolved}
			case in.Restore:
				content = "The user restarted di and asked to restore its review threads. Call restore_comments with every di review thread " +
					"from this conversation that the user has not resolved and that is not already in list_comments. " +
					"Give each thread its original id, file, side, line, line_text, context and full message history (user and claude), in order."
				meta = map[string]string{"kind": "restore"}
			}
			r.mu.Unlock()
			params, err := json.Marshal(map[string]any{"content": content, "meta": meta})
			if err == nil {
				err = out.Write(ctx, &jsonrpc.Request{Method: "notifications/claude/channel", Params: params})
			}
			if err != nil {
				log.Print(err)
			}
		}

		// The TUI exited; its threads are gone with it.
		r.mu.Lock()
		r.tui, r.threads = nil, nil
		r.mu.Unlock()
		c.Close()
	}
}

// upsert caches t for list_comments. r.mu must be held.
func (r *relay) upsert(t thread.Thread) {
	if i := slices.IndexFunc(r.threads, func(c thread.Thread) bool { return c.ID == t.ID }); i >= 0 {
		r.threads[i] = t
	} else {
		r.threads = append(r.threads, t)
	}
}

func format(t thread.Thread) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review thread %s on %s:%d (%s side)\n\n```diff\n%s\n```\n\n", t.ID, t.File, t.Line, t.Side, t.Context)
	var latest string
	for _, m := range t.Messages {
		fmt.Fprintf(&b, "%s: %s\n", m.Author, m.Body)
		if m.Author == "user" {
			latest = m.Body
		}
	}
	fmt.Fprintf(&b, "\nLatest user message:\n%s", latest)
	return b.String()
}
