// Package tui implements the diff review UI. It also owns the review threads and
// serves them to `di mcp` over a unix socket.
package tui

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/charmbracelet/x/ansi"
	"github.com/horita-yuya/di/internal/config"
	"github.com/horita-yuya/di/internal/git"
	"github.com/horita-yuya/di/internal/thread"
)

var (
	// Light theme only (GitHub light palette); Run switches the terminal to a white background.
	addStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#1a7f37"))
	delStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	addLineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#116329")).Background(lipgloss.Color("#e6ffec"))
	delLineStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#82071e")).Background(lipgloss.Color("#ffebe9"))
	hunkStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#0969da")).Background(lipgloss.Color("#ddf4ff"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#424a53"))
	threadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7d4e00")).Background(lipgloss.Color("#fff8c5"))
	folderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#54aeff"))
	fileStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#59636e"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8c959f"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	cursorBg     = lipgloss.Color("#b6e3ff")
	buttonStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#1f883d"))
	aiStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#8250df"))
	cancelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1f2328")).Background(lipgloss.Color("#eaeef2"))

	sides = map[string]string{"old": "L", "new": "R"} // GitHub's left/right line notation
)

// codeX is where a code line's text starts in the diff pane: the 2-cell gutter, then "%4s %4s " line numbers.
const codeX = 12

type rowKind int

const (
	rowFile rowKind = iota
	rowHunk
	rowLine
	rowThread
	rowComment // the Comment button closing a thread
)

type row struct {
	kind     rowKind
	file     *gitdiff.File
	frag     *gitdiff.TextFragment
	idx      int    // index into frag.Lines for rowLine
	old, new int    // line numbers for rowLine (0 if absent on that side), positions for rowHunk
	thread   string // thread ID for rowThread and rowComment
	text     string
}

type lineKey struct {
	side string
	line int
}

// key is the (side, line) a thread on this code line is attached to.
func (r row) key() lineKey {
	if r.new == 0 {
		return lineKey{"old", r.old}
	}
	return lineKey{"new", r.new}
}

type node struct {
	path, name string
	depth      int
	dir        bool
	file       *gitdiff.File
}

type loadedMsg struct {
	cfg    config.RepoConfig
	branch string
	raw    string
	files  []*gitdiff.File
	err    error
	gen    int
}

// peerMsg is a newly connected `di mcp` (one per Claude Code session); peerGoneMsg is one that disconnected.
type (
	peerMsg     struct{ conn net.Conn }
	peerGoneMsg struct{ conn net.Conn }
)

// pushedMsg is the result of Commit & Push.
type pushedMsg struct{ err error }

// prMsg is the result of Open PR.
type prMsg struct{ err error }

type model struct {
	repo  *git.Repo
	slug  string
	cfg   config.RepoConfig
	base  string
	bases []string // diff targets selectable in the footer: the configured base, origin/main and HEAD (uncommitted changes)
	gen   int      // bumped on base change so in-flight loads for the old base are dropped

	branch string
	raw    string
	files  []*gitdiff.File
	err    error

	threads     []thread.Thread
	seq         int               // last thread ID; IDs stay unique after a thread is resolved
	status      map[string]string // thread ID → Claude's latest set_status, until it replies
	restoreNote string            // feedback after the Restore button, e.g. " restored 2"
	peers       []net.Conn

	collapsed map[string]bool // file path → folded
	open      map[string]bool // dir path → expanded

	rows               []row
	tree               []node // visible nodes
	offset, treeOffset int    // first visible row / tree node
	mouseX, mouseY     int
	current            string // file highlighted in the tree

	editing bool
	commit  bool   // the textarea holds a commit message instead of a comment
	replyTo string // thread ID being commented on; "" for a new thread on target
	target  row
	input   textarea.Model

	pushing bool
	pushErr error // Commit & Push / Open PR error, kept apart from err, which every reload overwrites

	width, height int
}

func Run(repo *git.Repo, sock, slug string, cfg config.RepoConfig, base string) error {
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
		return errors.New("di is already running in this worktree")
	}
	os.Remove(sock) // stale socket from a crashed di
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer ln.Close() // also removes the socket file

	input := textarea.New()
	input.SetStyles(textarea.DefaultLightStyles())
	input.SetVirtualCursor(false) // View places the real cursor, where the terminal draws IME preedit
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	input.ShowLineNumbers = false
	input.SetHeight(5)
	p := tea.NewProgram(model{repo: repo, slug: slug, cfg: cfg, base: base, bases: slices.Compact([]string{base, "origin/main", "HEAD"}), input: input,
		collapsed: map[string]bool{}, open: map[string]bool{}, status: map[string]string{},
		seq: int(time.Now().Unix())}) // new IDs stay above those of threads restored from an earlier run
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.Send(peerMsg{c})
			go func() {
				defer func() { c.Close(); p.Send(peerGoneMsg{c}) }()
				for dec := json.NewDecoder(c); ; {
					var msg thread.ToTUI
					if dec.Decode(&msg) != nil {
						return
					}
					p.Send(msg)
				}
			}()
		}
	}()
	_, err = p.Run()
	return err
}

func (m model) load() tea.Msg {
	files, raw, err := m.repo.Diff(m.base)
	slices.SortFunc(files, func(a, b *gitdiff.File) int { return strings.Compare(path(a), path(b)) })
	cfg := m.cfg
	if c, err := config.Load(); err == nil {
		cfg = c.Repo(m.slug) // pick up edits, e.g. set_collapsed from Claude
	}
	return loadedMsg{cfg: cfg, branch: m.repo.Branch(), raw: raw, files: files, err: err, gen: m.gen}
}

func (m model) Init() tea.Cmd { return m.load }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(map[bool]int{false: m.diffWidth() - codeX, true: m.width}[m.commit])
		m.build() // rewraps threads
	case loadedMsg:
		if msg.gen != m.gen {
			return m, nil // stale load; also ends its tick chain
		}
		next := tea.Tick(time.Second, func(time.Time) tea.Msg { return m.load() })
		m.branch, m.err = cmp.Or(msg.branch, m.branch), msg.err
		cfgChanged := !slices.Equal(msg.cfg.CollapsedGlobs, m.cfg.CollapsedGlobs)
		if msg.err != nil || (!cfgChanged && msg.raw == m.raw && m.rows != nil) {
			return m, next
		}
		if cfgChanged {
			// New collapse defaults: drop fold state so build re-derives it.
			m.cfg = msg.cfg
			clear(m.collapsed)
			clear(m.open)
		}
		m.raw, m.files = msg.raw, msg.files
		m.build()
		return m, next
	case peerMsg:
		m.peers = append(m.peers, msg.conn)
		for _, t := range m.threads {
			if t.Messages[len(t.Messages)-1].Author == "user" {
				json.NewEncoder(msg.conn).Encode(thread.ToMCP{Thread: &t})
			}
		}
	case peerGoneMsg:
		m.peers = slices.DeleteFunc(m.peers, func(c net.Conn) bool { return c == msg.conn })
	case pushedMsg:
		m.pushing, m.pushErr = false, msg.err
		if msg.err == nil {
			m.gen++ // restarts the reload loop instead of starting a second one
			return m, m.load
		}
	case prMsg:
		m.pushErr = msg.err
	case thread.ToTUI:
		if r := msg.Reply; r != nil {
			if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == r.ID }); i >= 0 {
				m.threads[i].Messages = append(m.threads[i].Messages, thread.Message{Author: "claude", Body: r.Body})
			}
			delete(m.status, r.ID)
		}
		if s := msg.Status; s != nil && slices.ContainsFunc(m.threads, func(t thread.Thread) bool { return t.ID == s.ID }) {
			m.status[s.ID] = s.Text
		}
		if msg.Restore != nil {
			added := 0
			for _, t := range msg.Restore {
				n, _ := strconv.Atoi(t.ID)
				m.seq = max(m.seq, n)
				if len(t.Messages) > 0 && !slices.ContainsFunc(m.threads, func(u thread.Thread) bool { return u.ID == t.ID }) {
					m.threads = append(m.threads, t)
					added++
				}
			}
			// Restored threads awaiting Claude don't show in the panel, so it says what happened.
			m.restoreNote = fmt.Sprintf(" restored %d", added)
			if added == 0 {
				m.restoreNote = " nothing to restore"
			}
		}
		m.build()
	case tea.PasteMsg:
		if m.editing {
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.editing {
			if msg.String() == "enter" {
				cmd := m.submit()
				return m, cmd
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		// up/down move to the previous/next file visible in the tree (which is sorted by path).
		switch msg.String() {
		case "down":
			if i := slices.IndexFunc(m.tree, func(n node) bool { return !n.dir && n.path > m.current }); i >= 0 {
				m.jump(m.tree[i].path)
			}
		case "up":
			for _, n := range slices.Backward(m.tree) {
				if !n.dir && n.path < m.current {
					m.jump(n.path)
					break
				}
			}
		}
	case tea.MouseMsg:
		_, click := msg.(tea.MouseClickMsg)
		mouse := msg.Mouse()
		m.mouseX, m.mouseY = mouse.X, mouse.Y
		treeW, h, scr := m.width*3/10, m.bodyHeight(), m.screen()
		inTree := mouse.X < treeW
		// The input's buttons, " Comment " at x 0-8 and " Cancel " at x 11-18: under the body in commit mode,
		// on the inline box's last line (indented to the code text) in comment mode.
		bx, by := 0, h+lipgloss.Height(m.input.View())
		if !m.commit {
			bx, by = treeW+1+codeX, slices.Index(scr, -1)+lipgloss.Height(m.input.View())
		}
		switch {
		case mouse.Button == tea.MouseWheelUp || mouse.Button == tea.MouseWheelDown:
			d := 3
			if mouse.Button == tea.MouseWheelUp {
				d = -3
			}
			if inTree {
				m.treeOffset += d
			} else {
				m.offset += d
			}
			m.scroll()
			if !inTree && m.offset < len(m.rows) {
				m.current = path(m.rows[m.offset].file)
			}
		case !click || mouse.Button != tea.MouseLeft:
		case mouse.Y == m.height-1:
			// Base buttons in the footer, laid out as in View.
			x := lipgloss.Width(m.connection() + m.branch + " ← ")
			for _, b := range m.bases {
				w := lipgloss.Width(" " + b + " ")
				if mouse.X >= x && mouse.X < x+w && b != m.base {
					m.base = b
					m.gen++
					return m, m.load
				}
				x += w + 1
			}
			x++ // two spaces before the commit button
			if !m.pushing && mouse.X >= x && mouse.X < x+lipgloss.Width(" Commit & Push ") {
				m.editing, m.commit = true, true
				m.input.Placeholder = "Commit message (enter: commit & push, empty: timestamp)"
				m.input.SetWidth(m.width)
				m.scroll()
				return m, m.input.Focus()
			}
			x += lipgloss.Width(map[bool]string{false: " Commit & Push ", true: " Pushing… "}[m.pushing]) + 2
			if mouse.X >= x && mouse.X < x+lipgloss.Width(" Open PR ") {
				return m, func() tea.Msg { return prMsg{m.repo.OpenPR()} }
			}
		case m.editing && mouse.Y == by && (m.commit || slices.Contains(scr, -1)):
			switch x := mouse.X - bx; {
			case x >= 0 && x < 9:
				cmd := m.submit()
				return m, cmd
			case x >= 11 && x < 19:
				m.editing, m.commit = false, false
				m.input.Reset()
				m.scroll()
			}
		case mouse.Y >= h: // the commit message input
		case inTree && mouse.Y < m.treeHeight():
			if i := m.treeOffset + mouse.Y; i < len(m.tree) {
				n := m.tree[i]
				switch {
				case n.dir:
					m.open[n.path] = !m.open[n.path]
					m.build()
				case mouse.X < 3+2*n.depth: // on the file arrow (after the 1-cell cursor and the indent)
					m.collapsed[n.path] = !m.collapsed[n.path]
					m.build()
				default:
					m.jump(n.path)
				}
			}
		case inTree:
			// AI Replies panel: a title row, then 2 rows per thread with " Resolve " at x 0-8 on the first,
			// or " Restore " at x 0-8 when there are none.
			y, rs := mouse.Y-m.treeHeight()-1, m.replies()
			if len(rs) == 0 && y == 0 && mouse.X < 9 {
				if len(m.peers) == 0 {
					m.pushErr = errors.New("Claude not connected")
				} else {
					m.broadcast(thread.ToMCP{Restore: true})
					m.restoreNote = " waiting for AI…"
				}
			}
			if y < 0 || y/2 >= len(rs) {
				break
			}
			t := rs[y/2]
			if y%2 == 0 && mouse.X < 9 {
				m.threads = slices.DeleteFunc(m.threads, func(u thread.Thread) bool { return u.ID == t.ID })
				delete(m.status, t.ID)
				m.broadcast(thread.ToMCP{Resolved: t.ID})
				if m.editing && !m.commit && m.replyTo == t.ID {
					m.editing = false
					m.input.Reset()
				}
				m.build()
				break
			}
			m.jump(t.File)
			if i := slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowThread && r.thread == t.ID }); i >= 0 {
				m.offset = i
				m.scroll()
			}
		case mouse.Y < len(scr) && scr[mouse.Y] >= 0:
			i := scr[mouse.Y]
			switch r := m.rows[i]; r.kind {
			case rowFile:
				m.collapsed[path(r.file)] = !m.collapsed[path(r.file)]
				m.build()
			case rowComment, rowLine: // retargets an open textarea, keeping the draft
				m.editing, m.commit, m.replyTo, m.target = true, false, r.thread, r
				m.input.Placeholder = "Leave a comment (enter: send, alt+enter: newline)"
				m.input.SetWidth(m.diffWidth() - codeX)
				// r is the anchor now: scroll the box below it into view.
				if over := i - m.offset + 2 + lipgloss.Height(m.input.View()) - m.bodyHeight(); over > 0 {
					m.offset += over
				}
				m.scroll()
				return m, m.input.Focus()
			}
		}
	}
	return m, nil
}

// submit commits and pushes with the textarea content as message in commit mode; otherwise it adds
// the content to the thread being commented on, or starts a new one on target.
func (m *model) submit() tea.Cmd {
	m.editing = false
	body := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if m.commit {
		m.commit, m.pushing, m.pushErr = false, true, nil
		m.scroll()
		return func() tea.Msg {
			return pushedMsg{m.repo.CommitPush(cmp.Or(body, strconv.FormatInt(time.Now().Unix(), 10)))}
		}
	}
	if body == "" {
		m.scroll()
		return nil
	}
	user := thread.Message{Author: "user", Body: body}
	if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == m.replyTo }); i >= 0 {
		m.threads[i].Messages = append(m.threads[i].Messages, user)
		m.broadcast(thread.ToMCP{Thread: &m.threads[i]})
	} else {
		r := m.target
		k := r.key()
		var ctx []string
		for _, l := range r.frag.Lines[max(0, r.idx-8):min(len(r.frag.Lines), r.idx+9)] {
			ctx = append(ctx, strings.TrimSuffix(l.String(), "\n"))
		}
		m.seq++
		m.threads = append(m.threads, thread.Thread{
			ID:       strconv.Itoa(m.seq),
			File:     path(r.file),
			Side:     k.side,
			Line:     k.line,
			LineText: r.text,
			Context:  strings.Join(ctx, "\n"),
			Messages: []thread.Message{user},
		})
		m.broadcast(thread.ToMCP{Thread: &m.threads[len(m.threads)-1]})
	}
	m.build()
	return nil
}

// broadcast sends msg to every connected `di mcp`, dropping peers that went away.
func (m *model) broadcast(msg thread.ToMCP) {
	m.peers = slices.DeleteFunc(m.peers, func(c net.Conn) bool { return json.NewEncoder(c).Encode(msg) != nil })
}

// build regenerates rows and the visible tree from files, threads and fold state,
// keeping the same file/line at the top of the diff when possible.
func (m *model) build() {
	var at row
	var skip int // rows above the top one with the same identity, e.g. earlier lines of its thread
	// The old side is relative to the base, so it doesn't shift when the working tree changes.
	same := func(r row) bool {
		return path(r.file) == path(at.file) && r.kind == at.kind && r.thread == at.thread && r.old == at.old && (at.old != 0 || r.new == at.new)
	}
	if m.offset < len(m.rows) {
		at = m.rows[m.offset]
		skip = m.offset - slices.IndexFunc(m.rows, same)
	}

	m.rows = nil
	for _, f := range m.files {
		p := path(f)
		if _, ok := m.collapsed[p]; !ok {
			m.collapsed[p] = m.cfg.Collapsed(p)
		}
		m.rows = append(m.rows, row{kind: rowFile, file: f})
		if m.collapsed[p] {
			continue
		}
		var body []row
		lines := map[lineKey]string{}
		for _, frag := range f.TextFragments {
			old, new := int(frag.OldPosition), int(frag.NewPosition)
			body = append(body, row{kind: rowHunk, file: f, frag: frag, old: old, new: new, text: printable(frag.Header())})
			for i, l := range frag.Lines {
				r := row{kind: rowLine, file: f, frag: frag, idx: i, text: printable(strings.TrimSuffix(l.Line, "\n"))}
				if l.Old() {
					r.old, old = old, old+1
				}
				if l.New() {
					r.new, new = new, new+1
				}
				lines[r.key()] = r.text
				body = append(body, r)
			}
		}
		attached := map[lineKey][]thread.Thread{}
		for _, t := range m.threads {
			if t.File != p {
				continue
			}
			k := lineKey{t.Side, t.Line}
			if text, ok := lines[k]; ok && text == t.LineText {
				attached[k] = append(attached[k], t)
			} else {
				m.rows = append(m.rows, threadRows(f, t, printable(fmt.Sprintf("(outdated %s:%d) ", t.Side, t.Line)), m.status[t.ID], m.diffWidth()-codeX)...)
			}
		}
		for _, r := range body {
			m.rows = append(m.rows, r)
			if r.kind == rowLine {
				for _, t := range attached[r.key()] {
					m.rows = append(m.rows, threadRows(f, t, "", m.status[t.ID], m.diffWidth()-codeX)...)
				}
			}
		}
	}

	m.tree = nil
	var hidden string // prefix of a closed directory
	seen := map[string]bool{}
	for _, f := range m.files {
		parts := strings.Split(path(f), "/")
		for d := 1; d <= len(parts); d++ {
			n := node{path: strings.Join(parts[:d], "/"), name: printable(parts[d-1]), depth: d - 1, dir: d < len(parts)}
			if seen[n.path] {
				continue
			}
			seen[n.path] = true
			if hidden != "" && strings.HasPrefix(n.path, hidden) {
				continue
			}
			hidden = ""
			if n.dir {
				if _, ok := m.open[n.path]; !ok {
					// Directories whose files are all collapsed start closed.
					m.open[n.path] = slices.ContainsFunc(m.files, func(f *gitdiff.File) bool {
						return strings.HasPrefix(path(f), n.path+"/") && !m.cfg.Collapsed(path(f))
					})
				}
				if !m.open[n.path] {
					hidden = n.path + "/"
				}
			} else {
				n.file = f
			}
			m.tree = append(m.tree, n)
		}
	}

	if at.file != nil {
		i := slices.IndexFunc(m.rows, same)
		if i >= 0 {
			i += skip
		} else {
			i = m.header(path(at.file))
		}
		if i >= 0 {
			m.offset = i
		}
	}
	m.scroll()
}

// threadRows renders t as rows of at most width cells, wrapping message bodies. status is Claude's progress note.
func threadRows(f *gitdiff.File, t thread.Thread, label, status string, width int) []row {
	var lines []string
	switch {
	case t.Messages[len(t.Messages)-1].Author == "claude":
		lines = []string{"💬 [Claude replied] " + label}
	case status != "":
		lines = []string{"💬 [AI: " + strings.ReplaceAll(printable(status), "\n", " ") + "…] " + label}
	default:
		lines = []string{"💬 [waiting for Claude] " + label}
	}
	for _, msg := range t.Messages {
		who := "you: "
		if msg.Author == "claude" {
			who = "claude: "
		}
		for i, l := range strings.Split(ansi.Wrap(printable(msg.Body), width-2-len(who), ""), "\n") {
			if i > 0 {
				who = strings.Repeat(" ", len(who))
			}
			lines = append(lines, "  "+who+l)
		}
	}
	rows := make([]row, len(lines))
	for i, l := range lines {
		rows[i] = row{kind: rowThread, file: f, thread: t.ID, text: l}
	}
	return append(rows, row{kind: rowComment, file: f, thread: t.ID})
}

// header returns the row index of p's file header, or -1.
func (m *model) header(p string) int {
	return slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowFile && path(r.file) == p })
}

// jump unfolds file p, scrolls the diff to its header and keeps it visible in the tree.
func (m *model) jump(p string) {
	m.current = p
	m.collapsed[p] = false
	m.build()
	m.offset = m.header(p)
	if i := slices.IndexFunc(m.tree, func(n node) bool { return n.path == p }); i >= 0 {
		m.treeOffset = max(min(m.treeOffset, i), i-m.treeHeight()+1)
	}
	m.scroll()
}

// selected reports whether r is what the open textarea comments on.
func (m model) selected(r row) bool {
	if !m.editing || m.commit {
		return false
	}
	if m.replyTo != "" {
		return r.thread == m.replyTo
	}
	return r.kind == rowLine && path(r.file) == path(m.target.file) && r.key() == m.target.key()
}

// scroll clamps the offsets so neither pane scrolls past its end.
func (m *model) scroll() {
	n := len(m.rows)
	if m.editing && !m.commit {
		n += lipgloss.Height(m.input.View()) + 1 // room for the inline box below the last row
	}
	m.offset = max(0, min(m.offset, n-m.bodyHeight()))
	m.treeOffset = max(0, min(m.treeOffset, len(m.tree)-m.treeHeight()))
}

func (m model) bodyHeight() int {
	h := m.height - 1
	if m.commit {
		h -= lipgloss.Height(m.input.View()) + 1 // + buttons
	}
	return max(1, h)
}

// screen maps each line of the diff pane to a row index, or -1 for the lines of the inline comment box,
// which follows its anchor: the target code line, or the Comment button of the thread being replied to.
func (m model) screen() []int {
	anchor := slices.IndexFunc(m.rows, func(r row) bool { return m.selected(r) && r.kind != rowThread })
	var s []int
	for i := m.offset; i < len(m.rows) && len(s) < m.bodyHeight(); i++ {
		s = append(s, i)
		if i == anchor {
			s = append(s, slices.Repeat([]int{-1}, lipgloss.Height(m.input.View())+1)...)
		}
	}
	return s[:min(len(s), m.bodyHeight())]
}

// replies are the threads waiting on the user: Claude wrote the last message.
func (m model) replies() []thread.Thread {
	return slices.DeleteFunc(slices.Clone(m.threads), func(t thread.Thread) bool {
		return t.Messages[len(t.Messages)-1].Author != "claude"
	})
}

// treeHeight is the tree's share of the left pane; the AI Replies panel takes the rest, up to half:
// a title row, then 2 rows per reply or the Restore row when there are none.
func (m model) treeHeight() int {
	h := m.bodyHeight()
	return h - min(max(2, 1+2*len(m.replies())), h/2)
}

func (m model) diffWidth() int { return m.width - m.width*3/10 - 1 }

func (m model) View() tea.View {
	// Light theme only: the terminal's default fg/bg are switched while di runs.
	v := tea.View{AltScreen: true, MouseMode: tea.MouseModeAllMotion,
		ForegroundColor: lipgloss.Color("#1f2328"), BackgroundColor: lipgloss.Color("#ffffff")}
	if m.width == 0 {
		return v
	}
	h := m.bodyHeight()
	treeW := m.width * 3 / 10
	diffW := m.diffWidth()

	rs, th := m.replies(), m.treeHeight()
	var tree []string
	for _, n := range m.tree[m.treeOffset:min(len(m.tree), m.treeOffset+th)] {
		cur := !n.dir && n.path == m.current
		hl := slices.ContainsFunc(rs, func(t thread.Thread) bool {
			return t.File == n.path || n.dir && !m.open[n.path] && strings.HasPrefix(t.File, n.path+"/")
		})
		plain := cur || hl // inner colors would cut the row background
		text := strings.Repeat("  ", n.depth)
		st := lipgloss.NewStyle()
		switch {
		case n.dir:
			arrow, icon := "▸ ", "\uf07b " // Nerd Font folder / folder-open
			if m.open[n.path] {
				arrow, icon = "▾ ", "\uf07c "
			}
			if !plain {
				icon = folderStyle.Render(icon)
			}
			text += arrow + icon + n.name
		default:
			arrow, icon := "▾ ", "\uf016 " // Nerd Font file; the arrow folds its diff
			if m.collapsed[n.path] {
				arrow = "▸ "
			}
			if !plain {
				icon = fileStyle.Render(icon)
			}
			add, del := stat(n.file)
			text += arrow + icon + n.name + "  "
			if plain {
				text += fmt.Sprintf("+%d -%d", add, del)
			} else {
				text += addStyle.Render(fmt.Sprintf("+%d", add)) + " " + delStyle.Render(fmt.Sprintf("-%d", del))
			}
			if m.collapsed[n.path] {
				st = dimStyle
			}
		}
		if hl {
			st = threadStyle
		}
		if cur {
			st = st.Foreground(lipgloss.Color("#1f2328")).Background(cursorBg)
			text = "▌" + text
		} else {
			text = " " + text
		}
		tree = append(tree, st.Inline(true).Width(treeW).MaxWidth(treeW).Render(text))
	}
	w := max(0, treeW-9)
	tree = append(tree, slices.Repeat([]string{""}, th-len(tree))...)
	tree = append(tree, aiStyle.Inline(true).Width(treeW).MaxWidth(treeW).Render(fmt.Sprintf(" AI Replies (%d)", len(rs))))
	if len(rs) == 0 {
		tree = append(tree, buttonStyle.Render(" Restore ")+dimStyle.Inline(true).MaxWidth(w).Render(cmp.Or(m.restoreNote, " ask Claude to restore threads")))
	}
	for _, t := range rs {
		msg, _, _ := strings.Cut(t.Messages[len(t.Messages)-1].Body, "\n")
		tree = append(tree,
			buttonStyle.Render(" Resolve ")+threadStyle.Inline(true).Width(w).MaxWidth(w).Render(printable(fmt.Sprintf(" %s %s%d", t.File, sides[t.Side], t.Line))),
			dimStyle.Inline(true).MaxWidth(treeW).Render("  "+printable(msg)))
	}
	tree = tree[:h] // entries past the panel are not shown

	scr := m.screen()
	first := slices.Index(scr, -1) // the inline box's first line
	// The textarea and its buttons: inline in the diff in comment mode, under the body in commit mode.
	var box []string
	if m.editing {
		btn, hint := " Commit ", "  → commit all changes & push"
		if !m.commit {
			k := m.target.key()
			t := thread.Thread{File: path(m.target.file), Side: k.side, Line: k.line}
			if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == m.replyTo }); i >= 0 {
				t = m.threads[i]
			}
			btn, hint = " Comment ", printable(fmt.Sprintf("  → %s %s%d", t.File, sides[t.Side], t.Line))
		}
		box = append(strings.Split(m.input.View(), "\n"), buttonStyle.Render(btn)+"  "+cancelStyle.Render(" Cancel ")+dimStyle.Render(hint))
		if v.Cursor = m.input.Cursor(); v.Cursor != nil {
			switch {
			case m.commit:
				v.Cursor.Y += h // the textarea starts right below the body
			case first < 0 || first+v.Cursor.Y >= h: // the box is scrolled out
				v.Cursor = nil
			default:
				v.Cursor.X += treeW + 1 + codeX
				v.Cursor.Y += first
			}
		}
	}
	hover := -1
	if m.mouseX >= treeW && m.mouseY < len(scr) {
		hover = scr[m.mouseY]
	}
	var diff []string
	for y, i := range scr {
		if i < 0 {
			diff = append(diff, ansi.Truncate(strings.Repeat(" ", codeX)+box[y-first], diffW, ""))
			continue
		}
		r := m.rows[i]
		var text string
		st := lipgloss.NewStyle()
		pre, btn := "  ", "" // gutter, button after it
		switch r.kind {
		case rowFile:
			st = headerStyle
			name := printable(path(r.file))
			if r.file.IsRename {
				name = printable(r.file.OldName + " → " + r.file.NewName)
			}
			switch add, del := stat(r.file); {
			case m.collapsed[path(r.file)]:
				text = "▶ " + name
			case r.file.IsBinary:
				text = "▼ " + name + "  Binary file"
			default:
				text = fmt.Sprintf("▼ %s  +%d -%d", name, add, del)
			}
		case rowHunk:
			st, text = hunkStyle, r.text
		case rowLine:
			op := r.frag.Lines[r.idx].Op
			text = fmt.Sprintf("%4s %4s %s%s", num(r.old), num(r.new), op, r.text)
			if i == hover || m.selected(r) {
				pre, btn = "", "+ "
			}
			switch op {
			case gitdiff.OpAdd:
				st = addLineStyle
			case gitdiff.OpDelete:
				st = delLineStyle
			}
		case rowThread:
			st, text = threadStyle, strings.Repeat(" ", 10)+r.text
		case rowComment:
			st, pre, btn = threadStyle, pre+strings.Repeat(" ", 12), " Comment "
		}
		if m.selected(r) {
			st = st.Bold(true).Background(cursorBg)
		}
		// Rendered in pieces: an inner style would cut the line background.
		w := diffW - lipgloss.Width(pre+btn)
		diff = append(diff, st.Inline(true).Render(pre)+buttonStyle.Render(btn)+
			st.Inline(true).Width(w).MaxWidth(w).Render(strings.ReplaceAll(text, "\t", "    ")))
	}
	if len(m.rows) == 0 {
		diff = append(diff, dimStyle.Render("no changes against "+m.base))
	}

	var add, del int
	for _, f := range m.files {
		a, d := stat(f)
		add, del = add+a, del+d
	}
	footer := m.connection() + m.branch + " ←"
	for _, b := range m.bases {
		st := cancelStyle
		if b == m.base {
			st = buttonStyle
		}
		footer += " " + st.Render(" "+b+" ")
	}
	footer += "  " + buttonStyle.Render(map[bool]string{false: " Commit & Push ", true: " Pushing… "}[m.pushing]) + "  " + buttonStyle.Render(" Open PR ")
	footer += fmt.Sprintf("  %d files  +%d -%d", len(m.files), add, del)
	if m.err != nil {
		footer += "  │ " + errStyle.Render(printable(m.err.Error()))
	}
	if m.pushErr != nil {
		footer += "  │ " + errStyle.Render(printable(m.pushErr.Error()))
	}

	view := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(treeW).Height(h).Render(strings.Join(tree, "\n")),
		dimStyle.Render(strings.Repeat("│\n", h-1)+"│"),
		lipgloss.NewStyle().Width(diffW).Height(h).Render(strings.Join(diff, "\n")),
	)
	if m.commit {
		view += "\n" + strings.Join(box, "\n")
	}
	v.Content = view + "\n" + lipgloss.NewStyle().Inline(true).MaxWidth(m.width).Render(footer)
	return v
}

// connection is the footer's leading Claude connection indicator.
func (m model) connection() string {
	if len(m.peers) > 0 {
		return addStyle.Bold(true).Render(fmt.Sprintf("● Claude connected (%d)", len(m.peers))) + "  "
	}
	return dimStyle.Render("○ Claude not connected") + "  "
}

// printable replaces control characters (C0 but tab and newline, DEL, C1) in untrusted text, such as diff lines,
// file names, Claude's replies and git errors, with '�' so it can't drive the terminal.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' && r != '\n' || r >= 0x7f && r <= 0x9f {
			return '�'
		}
		return r
	}, s)
}

// path is the new path, or the old path for deleted files.
func path(f *gitdiff.File) string { return cmp.Or(f.NewName, f.OldName) }

func stat(f *gitdiff.File) (add, del int) {
	for _, frag := range f.TextFragments {
		add, del = add+int(frag.LinesAdded), del+int(frag.LinesDeleted)
	}
	return add, del
}

func num(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}
