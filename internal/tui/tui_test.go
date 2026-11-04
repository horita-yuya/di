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

// click presses and releases the left button at (x, y).
func click(m model, x, y int) model {
	m = update(m, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	return update(m, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
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
	m.editing, m.direct = true, true
	gen := m.gen
	m = click(m, 2, 2)
	if m.worktree != other || m.gen == gen || m.editing || m.direct || m.header("gen/b.pb.go") < 0 {
		t.Fatalf("not switched: editing=%v direct=%v rows=%d", m.editing, m.direct, len(m.rows))
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

func TestMessageAI(t *testing.T) {
	m, sent := newModel(t)
	diffX, h := m.width*3/10+1, m.bodyHeight()
	at := func(m model, y int) string {
		return ansi.Cut(ansi.Strip(strings.Split(m.View().Content, "\n")[y]), diffX, m.width)
	}
	// By default the diff takes the right pane but for the section's title at its bottom.
	if m.diffHeight() != h-1 || !strings.HasPrefix(at(m, h-1), "▶ Message AI") {
		t.Fatalf("diffHeight=%d title %q", m.diffHeight(), at(m, h-1))
	}
	// Clicking the title opens it with the input focused, and the diff shrinks.
	m = click(m, diffX+2, h-1)
	if !m.chatOpen || !m.direct || !m.editing || m.diffHeight() > (h+1)/2 || slices.Contains(m.screen(), -1) {
		t.Fatalf("open=%v direct=%v diffHeight=%d", m.chatOpen, m.direct, m.diffHeight())
	}
	if !strings.HasPrefix(at(m, m.diffHeight()), "▼ Message AI") || !strings.HasPrefix(at(m, h-1), "   Send ") {
		t.Fatalf("title %q, last %q", at(m, m.diffHeight()), at(m, h-1))
	}
	if w := lipgloss.Width(m.input.View()); w != m.diffWidth()-2 {
		t.Fatalf("input width %d", w)
	}
	m.input.SetValue("how is it going?")
	m = click(m, diffX+3, h-1) // " Send "
	if got := <-sent; got.Message != "how is it going?" || got.Thread != nil {
		t.Fatalf("broadcast %+v", got)
	}
	if !m.direct || m.input.Value() != "" || len(m.threads) != 0 || len(m.chat) != 1 {
		t.Fatalf("direct=%v value=%q threads=%v chat=%v", m.direct, m.input.Value(), m.threads, m.chat)
	}
	// Claude's progress and reply show in the section, not in AI Replies.
	m = update(m, peerData{m.worktree, thread.ToTUI{Status: &thread.Status{ID: chatID, Text: "thinking"}}})
	if !strings.Contains(at(m, m.diffHeight()), "[AI: thinking…]") {
		t.Fatalf("title %q", at(m, m.diffHeight()))
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Reply: &thread.Reply{ID: chatID, Body: "fine"}}})
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "you: how is it going?") || !strings.Contains(v, "claude: fine") || len(m.replies()) != 0 || m.chatUnread {
		t.Fatalf("unread=%v replies=%v view:\n%s", m.chatUnread, m.replies(), v)
	}
	// Closed, a reply is flagged on the title.
	m = click(m, diffX+2, m.diffHeight())
	if m.chatOpen || m.editing || m.diffHeight() != h-1 {
		t.Fatalf("open=%v editing=%v", m.chatOpen, m.editing)
	}
	m = update(m, peerData{m.worktree, thread.ToTUI{Reply: &thread.Reply{ID: chatID, Body: "more"}}})
	if !strings.Contains(at(m, h-1), "● Claude replied") {
		t.Fatalf("title %q", at(m, h-1))
	}
	// Commenting on a line closes the section; reopened, the conversation is still there.
	m = click(m, diffX+2, h-1)
	m = click(m, diffX+codeX+2, lineY(t, m, func(r row) bool { return r.kind == rowLine && r.text == "TWO" }))
	if m.direct || !m.editing || m.chatOpen || m.diffHeight() != h-1 {
		t.Fatalf("direct=%v editing=%v open=%v", m.direct, m.editing, m.chatOpen)
	}
	if m = click(m, diffX+2, h-1); !m.direct || !m.input.Focused() || len(m.chat) != 3 {
		t.Fatalf("direct=%v chat=%v", m.direct, m.chat)
	}
	// While the textarea is elsewhere, a click anywhere in the open section brings it back.
	m.editing, m.direct = true, false
	if m = click(m, diffX+20, m.diffHeight()+2); !m.direct || !m.input.Focused() {
		t.Fatal("click in the conversation did not bring the input back")
	}
	// Without Claude the draft stays.
	m.peers = nil
	m.input.SetValue("hello")
	m = update(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pushErr == nil || m.input.Value() != "hello" || len(m.chat) != 3 || m.chatUnread {
		t.Fatalf("pushErr=%v value=%q chat=%v", m.pushErr, m.input.Value(), m.chat)
	}
	// The conversation comes back with Restore.
	m.chat = nil
	m = update(m, peerData{m.worktree, thread.ToTUI{Restore: []thread.Thread{{ID: chatID, Messages: []thread.Message{{Author: "user", Body: "hi"}}}}}})
	if len(m.chat) != 1 || len(m.threads) != 0 {
		t.Fatalf("chat=%v threads=%v", m.chat, m.threads)
	}
}

func TestCommitDialog(t *testing.T) {
	m, _ := newModel(t)
	footer := strings.Split(ansi.Strip(m.View().Content), "\n")[m.height-1]
	m = click(m, lipgloss.Width(footer[:strings.Index(footer, "Commit & Push")]), m.height-1)
	if !m.commit || m.bodyHeight() != m.height-1 {
		t.Fatalf("commit=%v bodyHeight=%d", m.commit, m.bodyHeight())
	}
	// The dialog sits on top of the body, centered.
	dx, dy, box := m.dialog()
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if len(lines) != m.height || !strings.Contains(lines[dy+1], "Commit & Push") || !strings.Contains(lines[dy+len(box)-2], " Commit    Cancel ") {
		t.Fatalf("dialog not drawn:\n%s", strings.Join(lines, "\n"))
	}
	if dx <= 0 || dx+lipgloss.Width(box[0]) >= m.width {
		t.Fatalf("dialog at x %d, width %d", dx, lipgloss.Width(box[0]))
	}
	// It is modal: clicks elsewhere do nothing, Cancel closes it.
	diffX := m.width*3/10 + 1
	m = click(m, diffX+codeX+2, lineY(t, m, func(r row) bool { return r.kind == rowLine && r.text == "TWO" }))
	if !m.commit || len(m.threads) != 0 {
		t.Fatal("click went through the dialog")
	}
	if m = click(m, dx+2+12, dy+len(box)-2); m.commit || m.editing {
		t.Fatalf("not cancelled: commit=%v", m.commit)
	}
}

func TestSelectCopies(t *testing.T) {
	m, _ := newModel(t)
	diffX := m.width*3/10 + 1
	y := lineY(t, m, func(r row) bool { return r.kind == rowLine && r.text == "TWO" })
	// Dragging over code text selects it without the line numbers and does not open a comment.
	m = update(m, tea.MouseClickMsg{X: diffX + codeX + 1, Y: y - 1, Button: tea.MouseLeft})
	m = update(m, tea.MouseMotionMsg{X: diffX + codeX + 3, Y: y, Button: tea.MouseLeft})
	next, cmd := m.Update(tea.MouseReleaseMsg{X: diffX + codeX + 3, Y: y, Button: tea.MouseLeft})
	m = next.(model)
	if m.editing || cmd == nil {
		t.Fatalf("editing=%v cmd=%v", m.editing, cmd)
	}
	if got := m.selectedText(); got != "two\nTWO" {
		t.Fatalf("selected %q", got)
	}
	if v := m.View().Content; !strings.Contains(v, selStyle.Render("TWO")) {
		t.Fatalf("selection not highlighted:\n%s", v)
	}
	// The next click clears it.
	if m = click(m, diffX+codeX+2, 0); m.selSpans() != nil {
		t.Fatal("selection kept")
	}
}

func TestURLAt(t *testing.T) {
	m, _ := newModel(t)
	url := "https://example.com/" + strings.Repeat("a", 120)
	m.threads = []thread.Thread{{ID: "1", File: "a.go", Side: "new", Line: 2, LineText: "TWO",
		Messages: []thread.Message{{Author: "user", Body: "see"}, {Author: "claude", Body: "look at " + url + "."}}}}
	m.build()
	diffX := m.width*3/10 + 1
	y := lineY(t, m, func(r row) bool { return r.kind == rowThread && strings.Contains(r.text, "https://") })
	x := diffX + strings.Index(ansi.Strip(strings.Split(m.View().Content, "\n")[y])[diffX:], "https://") + 2
	// The URL wraps on screen; clicking its start gives the whole of it.
	if got := m.urlAt(x, y); got != url {
		t.Fatalf("urlAt = %q", got)
	}
	if got := m.urlAt(diffX+2, y); got != "" {
		t.Fatalf("urlAt off the URL = %q", got)
	}
	// URLs are drawn as links, keeping the thread's background, without the period after them.
	link := threadStyle.Inline(true).Foreground(linkColor).Underline(true)
	if v := m.View().Content; !strings.Contains(v, link.Render("https://example.com/")) {
		t.Fatalf("URL not drawn as a link:\n%q", v)
	}
	if got := links(lipgloss.NewStyle(), "see https://x.y/z."); got != "see "+lipgloss.NewStyle().Foreground(linkColor).Underline(true).Render("https://x.y/z")+"." {
		t.Fatalf("links = %q", got)
	}
}
