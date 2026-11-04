package tui

import (
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/charmbracelet/x/ansi"
	"github.com/horita-yuya/di/internal/thread"
)

const diff = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
`

// newModel returns a 100x30 model showing diff, with a peer whose messages arrive on the returned channel.
func newModel(t *testing.T) (model, <-chan thread.ToMCP) {
	files, _, err := gitdiff.Parse(strings.NewReader(diff))
	if err != nil {
		t.Fatal(err)
	}
	tuiEnd, mcpEnd := net.Pipe()
	t.Cleanup(func() { tuiEnd.Close(); mcpEnd.Close() })
	sent := make(chan thread.ToMCP, 16)
	go func() {
		for dec := json.NewDecoder(mcpEnd); ; {
			var msg thread.ToMCP
			if dec.Decode(&msg) != nil {
				return
			}
			sent <- msg
		}
	}()
	m := model{files: files, input: textarea.New(), collapsed: map[string]bool{}, open: map[string]bool{}, peers: []net.Conn{tuiEnd}}
	m = update(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, sent
}

func update(m model, msg tea.Msg) model {
	next, _ := m.Update(msg)
	return next.(model)
}

func click(m model, x, y int) model {
	return update(m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
}

// lineY is the diff pane line showing the row for which ok holds.
func lineY(t *testing.T, m model, ok func(row) bool) int {
	y := slices.IndexFunc(m.screen(), func(i int) bool { return i >= 0 && ok(m.rows[i]) })
	if y < 0 {
		t.Fatal("row not on screen")
	}
	return y
}

func TestCommentReplyResolveRestore(t *testing.T) {
	m, sent := newModel(t)
	diffX := m.width*3/10 + 1

	// Clicking a code line opens the box right below it.
	y := lineY(t, m, func(r row) bool { return r.kind == rowLine && r.text == "TWO" })
	m = click(m, diffX+5, y)
	if scr := m.screen(); scr[y+1] != -1 {
		t.Fatalf("box not below the line: %v", scr)
	}
	// The box starts where the code text does, after the gutter and line numbers.
	if w := lipgloss.Width(m.input.View()); w != m.diffWidth()-codeX {
		t.Fatalf("input width %d, want %d", w, m.diffWidth()-codeX)
	}
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if box := ansi.Cut(lines[y+1], diffX, m.width); !strings.HasPrefix(box, strings.Repeat(" ", codeX)) || strings.TrimSpace(box) == "" {
		t.Fatalf("box not indented to the code text: %q", box)
	}
	m.input.SetValue("why?")
	by := y + 1 + lipgloss.Height(m.input.View()) // the box's button line
	if m = click(m, diffX+2, by); !m.editing {
		t.Fatal("click left of the indented box sent the comment")
	}
	m = click(m, diffX+codeX+2, by) // " Comment "
	if m.editing || len(m.threads) != 1 {
		t.Fatalf("comment not sent: editing=%v threads=%v", m.editing, m.threads)
	}
	if got := <-sent; got.Thread == nil || got.Thread.Messages[0].Body != "why?" || got.Thread.LineText != "TWO" {
		t.Fatalf("broadcast %+v", got)
	}
	id := m.threads[0].ID

	// Claude's reply lists the thread under the tree.
	m = update(m, thread.ToTUI{Reply: &thread.Reply{ID: id, Body: "because"}})
	if len(m.replies()) != 1 || m.treeHeight() >= m.bodyHeight() {
		t.Fatalf("replies=%v treeHeight=%d", m.replies(), m.treeHeight())
	}
	if v := m.View().Content; !strings.Contains(v, "Claude replies (1)") || !strings.Contains(v, "because") {
		t.Fatalf("panel not rendered:\n%s", v)
	}

	// Resolve deletes the thread and tells Claude.
	m = click(m, 2, m.treeHeight()+1)
	if len(m.threads) != 0 {
		t.Fatalf("not resolved: %v", m.threads)
	}
	if got := <-sent; got.Resolved != id {
		t.Fatalf("broadcast %+v", got)
	}

	// Restore re-adds threads once, and new IDs stay above restored ones.
	restored := thread.Thread{ID: "9999999999", File: "a.go", Side: "new", Line: 2, LineText: "TWO",
		Messages: []thread.Message{{Author: "user", Body: "why?"}, {Author: "claude", Body: "because"}}}
	for range 2 {
		m = update(m, thread.ToTUI{Restore: []thread.Thread{restored, {ID: "1"}}})
	}
	if len(m.threads) != 1 || m.seq != 9999999999 {
		t.Fatalf("threads=%v seq=%d", m.threads, m.seq)
	}
	if !slices.ContainsFunc(m.rows, func(r row) bool { return r.kind == rowThread && r.thread == restored.ID }) {
		t.Fatal("restored thread not attached to its line")
	}

	// Replying from the thread's Comment button anchors the box on that button.
	y = lineY(t, m, func(r row) bool { return r.kind == rowComment })
	m = click(m, diffX+2, y)
	if m.replyTo != restored.ID || m.screen()[y+1] != -1 {
		t.Fatalf("replyTo=%q screen=%v", m.replyTo, m.screen())
	}
	// Resolving the thread being replied to closes the box instead of leaving a dangling target.
	m = click(m, 2, m.treeHeight()+1)
	if m.editing || len(m.threads) != 0 {
		t.Fatalf("editing=%v threads=%v", m.editing, m.threads)
	}
}

func TestRestoreButton(t *testing.T) {
	m, sent := newModel(t)
	// With no replies, the panel under the tree holds the Restore button; the footer no longer does.
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if m.treeHeight() != m.bodyHeight()-2 || !strings.HasPrefix(lines[m.treeHeight()+1], " Restore ") {
		t.Fatalf("treeHeight=%d panel row %q", m.treeHeight(), lines[m.treeHeight()+1])
	}
	if strings.Contains(lines[len(lines)-1], "Restore") {
		t.Fatalf("footer still has Restore: %q", lines[len(lines)-1])
	}
	m = click(m, 2, m.treeHeight()+1)
	if got := <-sent; !got.Restore {
		t.Fatalf("broadcast %+v", got)
	}
	m.peers = nil
	m = click(m, 2, m.treeHeight()+1)
	if m.pushErr == nil {
		t.Fatal("want an error without Claude")
	}
}
