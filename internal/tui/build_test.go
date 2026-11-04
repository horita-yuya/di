package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/charmbracelet/x/ansi"
	"github.com/horita-yuya/di/internal/config"
	"github.com/horita-yuya/di/internal/thread"
)

func parse(t *testing.T, raw string) []*gitdiff.File {
	t.Helper()
	files, _, err := gitdiff.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// modelOf returns a 100x30 model showing raw (files sorted by path, as load does), without Claude.
func modelOf(t *testing.T, raw string, cfg config.RepoConfig) model {
	m := model{raw: raw, files: parse(t, raw), cfg: cfg, input: textarea.New(), collapsed: map[string]bool{}, open: map[string]bool{}}
	return update(m, tea.WindowSizeMsg{Width: 100, Height: 30})
}

// reload is the hot reload delivering raw.
func reload(t *testing.T, m model, raw string) model {
	return update(m, loadedMsg{cfg: m.cfg, raw: raw, files: parse(t, raw), gen: m.gen})
}

func user(body string) []thread.Message { return []thread.Message{{Author: "user", Body: body}} }

func TestOutdatedThread(t *testing.T) {
	th := thread.Thread{ID: "1", File: "a.go", Side: "new", Line: 2, LineText: "TWO", Messages: user("why?")}
	for name, tc := range map[string]struct {
		raw      string
		outdated bool
	}{
		"unchanged":           {diff, false},
		"another line edited": {strings.Replace(diff, "@@ -1,3 +1,3 @@", "@@ -1,3 +1,4 @@", 1) + "+four\n", false},
		"its line edited":     {strings.Replace(diff, "+TWO", "+Two", 1), true},
		"its line moved":      {strings.Replace(diff, "@@ -1,3 +1,3 @@\n one", "@@ -1,3 +1,4 @@\n+zero\n one", 1), true},
	} {
		t.Run(name, func(t *testing.T) {
			m := modelOf(t, diff, config.RepoConfig{})
			m = update(m, thread.ToTUI{Restore: []thread.Thread{th}})
			if i := slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowLine && r.text == "TWO" }); m.rows[i+1].kind != rowThread {
				t.Fatalf("thread not below its line: %+v", m.rows[i+1])
			}

			m = reload(t, m, tc.raw)
			i := slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowThread })
			if gotOutdated := i == 1; gotOutdated != tc.outdated {
				t.Fatalf("thread at row %d, want outdated=%v", i, tc.outdated)
			}
			if label := strings.Contains(m.rows[i].text, "(outdated new:2)"); label != tc.outdated {
				t.Fatalf("label %q", m.rows[i].text)
			}
			if !tc.outdated && (m.rows[i-1].kind != rowLine || m.rows[i-1].text != "TWO") {
				t.Fatalf("thread not below its line after reload: %+v", m.rows[i-1])
			}
		})
	}
}

func TestFoldFile(t *testing.T) {
	m := modelOf(t, diff, config.RepoConfig{})
	diffX := m.width*3/10 + 1
	header := func(m model) string {
		y := lineY(t, m, func(r row) bool { return r.kind == rowFile })
		return strings.TrimSpace(ansi.Strip(ansi.Cut(strings.Split(m.View().Content, "\n")[y], diffX, m.width)))
	}
	if got := header(m); got != "▼ a.go  +1 -1" {
		t.Fatalf("header %q", got)
	}

	// Clicking the file header folds and unfolds the file.
	m = click(m, diffX+4, lineY(t, m, func(r row) bool { return r.kind == rowFile }))
	if len(m.rows) != 1 || header(m) != "▶ a.go" {
		t.Fatalf("not folded: rows=%d header=%q", len(m.rows), header(m))
	}
	m = click(m, diffX+4, lineY(t, m, func(r row) bool { return r.kind == rowFile }))
	if len(m.rows) == 1 || header(m) != "▼ a.go  +1 -1" {
		t.Fatalf("not unfolded: rows=%d header=%q", len(m.rows), header(m))
	}

	// In the tree, the arrow folds; the rest of the row jumps to the file, unfolding it.
	m = click(m, 1, 0)
	if !m.collapsed["a.go"] || len(m.rows) != 1 {
		t.Fatal("tree arrow did not fold")
	}
	m = click(m, 10, 0)
	if m.collapsed["a.go"] || len(m.rows) == 1 || m.current != "a.go" {
		t.Fatal("tree row did not jump and unfold")
	}
}

const twoDirs = `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1 @@
-a
+A
diff --git a/gen/b.pb.go b/gen/b.pb.go
--- a/gen/b.pb.go
+++ b/gen/b.pb.go
@@ -1 +1 @@
-b
+B
`

func TestRepliesInClosedDirectory(t *testing.T) {
	m := modelOf(t, twoDirs, config.RepoConfig{CollapsedGlobs: []string{"gen/**"}})
	names := func(m model) []string {
		var s []string
		for _, n := range m.tree {
			s = append(s, n.path)
		}
		return s
	}
	if got := names(m); !slices.Equal(got, []string{"a.go", "gen"}) || m.open["gen"] {
		t.Fatalf("tree %v, gen open=%v; want gen closed", got, m.open["gen"])
	}

	m = update(m, thread.ToTUI{Restore: []thread.Thread{{ID: "5", File: "gen/b.pb.go", Side: "new", Line: 1, LineText: "B",
		Messages: []thread.Message{{Author: "user", Body: "why?"}, {Author: "claude", Body: "generated\nsecond line"}}}}})

	// The closed directory holding the replied file is highlighted like the thread; other rows are not.
	hl, _, _ := strings.Cut(threadStyle.Render("x"), "x")
	lines := strings.Split(m.View().Content, "\n")
	treeW := m.width * 3 / 10
	for y, want := range []bool{false, true} {
		if got := strings.Contains(ansi.Cut(lines[y], 0, treeW), hl); got != want {
			t.Errorf("tree row %q highlighted=%v, want %v", m.tree[y].path, got, want)
		}
	}

	// The replies panel lists it with its first line, and clicking it jumps to the thread.
	panel := ansi.Strip(strings.Join(lines[m.treeHeight():m.treeHeight()+3], "\n"))
	for _, want := range []string{"Claude replies (1)", "gen/b.pb.go R1", "generated"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("panel lacks %q:\n%s", want, panel)
		}
	}
	if strings.Contains(panel, "second line") {
		t.Fatalf("panel shows more than the first line:\n%s", panel)
	}
	m = click(m, 12, m.treeHeight()+1)
	if m.current != "gen/b.pb.go" || m.collapsed["gen/b.pb.go"] {
		t.Fatalf("current=%q collapsed=%v", m.current, m.collapsed["gen/b.pb.go"])
	}
	lineY(t, m, func(r row) bool { return r.kind == rowThread && r.thread == "5" })

	// Once the user answers, it is no longer waiting on them.
	m.threads[0].Messages = append(m.threads[0].Messages, thread.Message{Author: "user", Body: "ok"})
	if v := ansi.Strip(m.View().Content); len(m.replies()) != 0 || !strings.Contains(v, "Claude replies (0)") || !strings.Contains(v, " Restore ") {
		t.Fatal("answered thread still listed, or the empty panel lacks Restore")
	}
}

// long is a 40-line file with line 2 changed, and with a line inserted at the top when inserted is set.
func long(inserted bool) string {
	var b strings.Builder
	n := 40
	if inserted {
		n++
	}
	fmt.Fprintf(&b, "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,40 +1,%d @@\n", n)
	if inserted {
		b.WriteString("+zero\n")
	}
	b.WriteString(" line 1\n-line 2\n+LINE 2\n")
	for i := 3; i <= 40; i++ {
		fmt.Fprintf(&b, " line %d\n", i)
	}
	return b.String()
}

func TestTopRowStays(t *testing.T) {
	th := thread.Thread{ID: "1", File: "a.go", Side: "new", Line: 2, LineText: "LINE 2", Messages: user("why?")}
	for name, change := range map[string]func(*testing.T, model) model{
		"reply": func(t *testing.T, m model) model {
			return update(m, thread.ToTUI{Reply: &thread.Reply{ID: "1", Body: "because\nof\nthis"}})
		},
		"code edit above": func(t *testing.T, m model) model { return reload(t, m, long(true)) },
	} {
		t.Run(name, func(t *testing.T) {
			m := modelOf(t, long(false), config.RepoConfig{})
			m = update(m, thread.ToTUI{Restore: []thread.Thread{th}})
			m.offset = slices.IndexFunc(m.rows, func(r row) bool { return r.text == "line 10" })
			m = change(t, m)
			if got := m.rows[m.offset].text; got != "line 10" {
				t.Fatalf("top row %q, want line 10", got)
			}
		})
	}
}
