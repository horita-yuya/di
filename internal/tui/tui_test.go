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
	"github.com/horita-yuya/di/internal/config"
	"github.com/horita-yuya/di/internal/git"
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
	m := model{worktree: &worktree{files: files, collapsed: map[string]bool{}, open: map[string]bool{}, status: map[string]string{}, peers: []net.Conn{tuiEnd}},
		input: textarea.New()}
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
	m = update(m, peerData{m.worktree, thread.ToTUI{Reply: &thread.Reply{ID: id, Body: "because"}}})
	if len(m.replies()) != 1 || m.treeHeight() >= m.bodyHeight() {
		t.Fatalf("replies=%v treeHeight=%d", m.replies(), m.treeHeight())
	}
	if v := m.View().Content; !strings.Contains(v, "AI Replies (1)") || !strings.Contains(v, "because") {
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
		m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{restored, {ID: "1"}}}})
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
	m = update(m, tea.WindowSizeMsg{Width: 200, Height: 30}) // room for the notes after the button
	panel := func(m model) string { return ansi.Strip(strings.Split(m.View().Content, "\n")[m.treeHeight()+1]) }
	if p := panel(m); !strings.Contains(p, "ask Claude to restore threads") {
		t.Fatalf("panel %q", p)
	}
	m = click(m, 2, m.treeHeight()+1)
	if got := <-sent; !got.Restore {
		t.Fatalf("broadcast %+v", got)
	}
	if p := panel(m); !strings.Contains(p, "waiting for AI…") {
		t.Fatalf("panel %q", p)
	}
	// A restored thread awaiting Claude is not in the panel, so the note says it arrived.
	waiting := thread.Thread{ID: "1", File: "a.go", Side: "new", Line: 2, LineText: "TWO", Messages: []thread.Message{{Author: "user", Body: "why?"}}}
	if m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{waiting}}}); !strings.Contains(panel(m), "restored 1") {
		t.Fatalf("panel %q", panel(m))
	}
	if m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{}}}); !strings.Contains(panel(m), "nothing to restore") {
		t.Fatalf("panel %q", panel(m))
	}
	m = click(m, 2, m.treeHeight()+1)
	if got := <-sent; !got.Restore || !strings.Contains(panel(m), "waiting for AI…") {
		t.Fatalf("broadcast %+v, panel %q", got, panel(m))
	}
	m.peers = nil
	m = click(m, 2, m.treeHeight()+1)
	if m.pushErr == nil {
		t.Fatal("want an error without Claude")
	}
}

func TestControlCharactersNotRendered(t *testing.T) {
	m := modelOf(t, strings.Replace(diff, "+TWO", "+TWO\x1b]52;c;ZGlmZg==\x07", 1), config.RepoConfig{})
	m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{{ID: "1", File: "a.go", Side: "new", Line: 1, LineText: "one",
		Messages: []thread.Message{{Author: "user", Body: "why?"}, {Author: "claude", Body: "ok\x1b]52;c;cmVwbHk=\x07\x9b2J"}}}}}})
	v := m.View().Content
	for _, payload := range []string{"]52;c;ZGlmZg==", "]52;c;cmVwbHk="} {
		if strings.Contains(v, "\x1b"+payload) || !strings.Contains(v, "�"+payload) {
			t.Errorf("%q not neutralized:\n%q", payload, v)
		}
	}
	if strings.Contains(v, "\x9b2J") {
		t.Error("C1 CSI reached the terminal")
	}
}

func TestStatus(t *testing.T) {
	m, _ := newModel(t)
	m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{{ID: "1", File: "a.go", Side: "new", Line: 2, LineText: "TWO", Messages: user("why?")}}}})
	header := func(m model) string {
		return m.rows[slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowThread })].text
	}
	if got := header(m); got != "💬 [waiting for Claude] " {
		t.Fatalf("header %q", got)
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Status: &thread.Status{ID: "2", Text: "ghost"}}})
	if _, ok := m.status["2"]; ok {
		t.Fatal("status kept for an unknown thread")
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Status: &thread.Status{ID: "1", Text: "reading\ntui.go"}}})
	if got := header(m); got != "💬 [AI: reading tui.go…] " || !strings.Contains(ansi.Strip(m.View().Content), "[AI: reading tui.go…]") {
		t.Fatalf("header %q", got)
	}

	// A reply clears it, and so does resolving.
	m = update(m, peerData{m.worktree, thread.ToTUI{Reply: &thread.Reply{ID: "1", Body: "because"}}})
	if _, ok := m.status["1"]; ok || header(m) != "💬 [Claude replied] " {
		t.Fatalf("status %v header %q", m.status, header(m))
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Status: &thread.Status{ID: "1", Text: "editing"}}})
	m = click(m, 2, m.treeHeight()+1) // Resolve
	if len(m.threads) != 0 || len(m.status) != 0 {
		t.Fatalf("threads %v status %v", m.threads, m.status)
	}
}

func TestWorktrees(t *testing.T) {
	m, sent := newModel(t)
	if v := ansi.Strip(m.View().Content); strings.Contains(v, "Worktrees") {
		t.Fatalf("a single worktree shows the section:\n%s", v)
	}
	// Thread IDs are per worktree, so both have a thread 7.
	waiting := thread.Thread{ID: "7", File: "a.go", Side: "new", Line: 2, LineText: "TWO", Messages: user("why?")}
	m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{waiting}}})
	other := &worktree{repo: &git.Repo{Root: "/src/other"}, branch: "feature", raw: twoDirs, files: parse(t, twoDirs),
		collapsed: map[string]bool{}, open: map[string]bool{}, status: map[string]string{}, threads: []thread.Thread{waiting}}
	m.repo, m.branch, m.wts = &git.Repo{Root: "/src/main"}, "main", []*worktree{m.worktree, other}
	m = update(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	lines := func(m model) []string { return strings.Split(ansi.Strip(m.View().Content), "\n") }
	wh := m.wtHeight()
	if l := lines(m); wh != 4 || !strings.HasPrefix(l[0], " Worktrees") || !strings.HasPrefix(l[1], "▌● main") || !strings.HasPrefix(l[2], " ○ feature") ||
		!strings.HasPrefix(l[3], " Files") {
		t.Fatalf("wtHeight=%d section:\n%s", wh, strings.Join(l[:4], "\n"))
	}
	if m = click(m, 2, 3); m.branch != "main" { // the Files title
		t.Fatal("clicking the Files title switched worktrees")
	}

	// A reply to the other worktree badges its row and leaves this one's thread 7 alone.
	m = update(m, peerData{other, thread.ToTUI{Reply: &thread.Reply{ID: "7", Body: "because"}}})
	if l := lines(m); !strings.HasPrefix(l[2], " ○ feature ●1") || len(m.threads[0].Messages) != 1 || len(m.replies()) != 0 {
		t.Fatalf("row %q, this worktree's thread %v", l[2], m.threads[0])
	}

	// The left pane below the section maps as before: the tree's file arrow, then the AI Replies panel.
	if m = click(m, 1, wh); !m.collapsed["a.go"] {
		t.Fatal("tree arrow did not fold")
	}
	if m = click(m, 2, wh+m.treeHeight()+1); !(<-sent).Restore {
		t.Fatal("Restore not clicked")
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Reply: &thread.Reply{ID: "7", Body: "ok"}}})
	if m = click(m, 2, wh+m.treeHeight()+1); len(m.threads) != 0 || (<-sent).Resolved != "7" {
		t.Fatalf("Resolve not clicked: %v", m.threads)
	}

	// The title and the current row do nothing; another row switches to that worktree, closing the textarea.
	for _, y := range []int{0, 1} {
		if m = click(m, 2, y); m.worktree == other {
			t.Fatalf("click on row %d switched", y)
		}
	}
	m.editing, m.commit = true, true
	gen := m.gen
	m = click(m, 2, 2)
	if m.worktree != other || m.gen == gen || m.editing || m.commit || m.header("gen/b.pb.go") < 0 {
		t.Fatalf("not switched: editing=%v commit=%v rows=%d", m.editing, m.commit, len(m.rows))
	}
	if l := lines(m); !strings.HasPrefix(l[1], " ● main") || !strings.HasPrefix(l[2], "▌○ feature ●1") ||
		!strings.Contains(l[len(l)-1], "○ Claude not connected  feature ←") {
		t.Fatalf("section / footer after switching:\n%s\n%s", strings.Join(l[:3], "\n"), l[len(l)-1])
	}
	// A load of the previous worktree, still in flight, is dropped.
	if m = update(m, loadedMsg{raw: diff, files: parse(t, diff), gen: gen}); m.raw != twoDirs {
		t.Fatal("stale load applied")
	}
}
