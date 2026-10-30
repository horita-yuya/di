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

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/horitayuya/di/internal/config"
	"github.com/horitayuya/di/internal/git"
	"github.com/horitayuya/di/internal/thread"
)

var (
	// Light theme only (GitHub light palette); Run switches the terminal to a white background.
	addStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#1a7f37"))
	delStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	addLineStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#116329")).Background(lipgloss.Color("#e6ffec"))
	delLineStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#82071e")).Background(lipgloss.Color("#ffebe9"))
	hunkStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("#0969da")).Background(lipgloss.Color("#ddf4ff"))
	headerStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1f2328")).Background(lipgloss.Color("#eaeef2"))
	threadStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#7d4e00")).Background(lipgloss.Color("#fff8c5"))
	folderStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#54aeff"))
	dimStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#8c959f"))
	errStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	cursorBg        = lipgloss.Color("#b6e3ff")
	treeCursorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#0969da"))
	bannerStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#bf8700"))
)

type rowKind int

const (
	rowFile rowKind = iota
	rowHunk
	rowLine
	rowThread
)

type row struct {
	kind     rowKind
	file     *gitdiff.File
	frag     *gitdiff.TextFragment
	idx      int    // index into frag.Lines for rowLine
	old, new int    // line numbers for rowLine (0 if absent on that side), positions for rowHunk
	thread   string // thread ID for rowThread
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
}

// peerMsg is a newly connected `di mcp` (one per Claude Code session); peerGoneMsg is one that disconnected.
type (
	peerMsg     struct{ conn net.Conn }
	peerGoneMsg struct{ conn net.Conn }
)

type model struct {
	repo *git.Repo
	slug string
	cfg  config.RepoConfig
	base string

	branch string
	raw    string
	files  []*gitdiff.File
	err    error

	threads []thread.Thread
	unread  map[string]bool // thread ID → Claude replied since the user last looked
	peers   []net.Conn

	collapsed map[string]bool // file path → folded
	open      map[string]bool // dir path → expanded

	rows                   []row
	tree                   []node // visible nodes
	cursor, offset         int
	treeCursor, treeOffset int
	focusTree              bool

	editing bool
	replyTo string // thread ID being replied to; "" for a new thread
	input   textarea.Model

	width, height int
}

func Run(repo *git.Repo, slug string, cfg config.RepoConfig, base string) error {
	sock := thread.SocketPath(repo.GitDir)
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
	input.Placeholder = "ctrl+s: send, esc: cancel"
	input.SetHeight(5)
	p := tea.NewProgram(model{repo: repo, slug: slug, cfg: cfg, base: base, input: input,
		unread: map[string]bool{}, collapsed: map[string]bool{}, open: map[string]bool{}}, tea.WithAltScreen(), tea.WithMouseCellMotion())
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
					var r thread.Reply
					if dec.Decode(&r) != nil {
						return
					}
					p.Send(r)
				}
			}()
		}
	}()
	// OSC 10/11 set the terminal's default fg/bg; OSC 110/111 restore them.
	fmt.Print("\x1b]10;#1f2328\x07\x1b]11;#ffffff\x07")
	defer fmt.Print("\x1b]110\x07\x1b]111\x07")
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
	return loadedMsg{cfg: cfg, branch: m.repo.Branch(), raw: raw, files: files, err: err}
}

func (m model) Init() tea.Cmd { return m.load }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width)
		m.scroll()
	case loadedMsg:
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
			if !t.Resolved && t.Messages[len(t.Messages)-1].Author == "user" {
				json.NewEncoder(msg.conn).Encode(t)
			}
		}
	case peerGoneMsg:
		m.peers = slices.DeleteFunc(m.peers, func(c net.Conn) bool { return c == msg.conn })
	case thread.Reply:
		i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == msg.ID })
		if i < 0 {
			return m, nil
		}
		m.threads[i].Messages = append(m.threads[i].Messages, thread.Message{Author: "claude", Body: msg.Body})
		m.threads[i].Resolved = m.threads[i].Resolved || msg.Resolve
		m.unread[msg.ID] = true
		m.build()
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.editing {
			return m.updateInput(msg)
		}
		var cmd tea.Cmd
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "tab":
			m.focusTree = !m.focusTree
		case "u":
			m.jumpUnread()
		default:
			if m.focusTree {
				m.updateTree(msg.String())
			} else {
				cmd = m.updateDiff(msg.String())
			}
		}
		m.scroll()
		return m, cmd
	case tea.MouseMsg:
		if m.editing || msg.Action != tea.MouseActionPress {
			return m, nil
		}
		inTree := msg.X < m.width*3/10
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			key := map[tea.MouseButton]string{tea.MouseButtonWheelUp: "k", tea.MouseButtonWheelDown: "j"}[msg.Button]
			if inTree {
				m.updateTree(key)
			} else {
				m.updateDiff(key)
			}
		case tea.MouseButtonLeft:
			y := msg.Y
			if len(m.unread) > 0 {
				y-- // banner
			}
			if y < 0 || y >= m.bodyHeight() {
				return m, nil
			}
			if inTree {
				m.treeCursor = m.treeOffset + y
				m.updateTree("enter")
			} else {
				m.cursor = m.offset + y
			}
		}
		m.scroll()
	}
	return m, nil
}

func (m model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.editing = false
		m.input.Reset()
		m.scroll()
		return m, nil
	case "ctrl+s":
		m.editing = false
		body := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		if body == "" {
			m.scroll()
			return m, nil
		}
		user := thread.Message{Author: "user", Body: body}
		if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == m.replyTo }); i >= 0 {
			m.threads[i].Messages = append(m.threads[i].Messages, user)
			m.threads[i].Resolved = false
			m.broadcast(m.threads[i])
		} else if m.replyTo == "" && m.cursor < len(m.rows) && m.rows[m.cursor].kind == rowLine {
			r := m.rows[m.cursor]
			k := r.key()
			var ctx []string
			for _, l := range r.frag.Lines[max(0, r.idx-8):min(len(r.frag.Lines), r.idx+9)] {
				ctx = append(ctx, strings.TrimSuffix(l.String(), "\n"))
			}
			m.threads = append(m.threads, thread.Thread{
				ID:       strconv.Itoa(len(m.threads) + 1),
				File:     path(r.file),
				Side:     k.side,
				Line:     k.line,
				LineText: r.text,
				Context:  strings.Join(ctx, "\n"),
				Messages: []thread.Message{user},
			})
			m.broadcast(m.threads[len(m.threads)-1])
		}
		m.build()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// broadcast sends t to every connected `di mcp`, dropping peers that went away.
func (m *model) broadcast(t thread.Thread) {
	m.peers = slices.DeleteFunc(m.peers, func(c net.Conn) bool { return json.NewEncoder(c).Encode(t) != nil })
}

func (m *model) updateDiff(key string) tea.Cmd {
	var cur row
	if m.cursor < len(m.rows) {
		cur = m.rows[m.cursor]
	}
	switch key {
	case "j", "down":
		m.cursor++
	case "k", "up":
		m.cursor--
	case "ctrl+d":
		m.cursor += m.bodyHeight() / 2
	case "ctrl+u":
		m.cursor -= m.bodyHeight() / 2
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = len(m.rows) - 1
	case "n":
		if i := slices.IndexFunc(m.rows[min(m.cursor+1, len(m.rows)):], func(r row) bool { return r.kind == rowFile }); i >= 0 {
			m.cursor += 1 + i
		}
	case "p":
		for i := m.cursor - 1; i >= 0; i-- {
			if m.rows[i].kind == rowFile {
				m.cursor = i
				break
			}
		}
	case "enter", " ", "x":
		if cur.file == nil || (key != "x" && cur.kind != rowFile) {
			return nil
		}
		p := path(cur.file)
		m.collapsed[p] = !m.collapsed[p]
		m.build()
		m.cursor = m.header(p)
	case "c", "r":
		if (key == "c" && cur.kind == rowLine) || (key == "r" && cur.kind == rowThread) {
			m.editing, m.replyTo = true, cur.thread
			return m.input.Focus()
		}
	case "R":
		if cur.kind == rowThread {
			i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == cur.thread })
			m.threads[i].Resolved = !m.threads[i].Resolved
			m.build()
		}
	}
	return nil
}

func (m *model) updateTree(key string) {
	switch key {
	case "j", "down":
		m.treeCursor++
	case "k", "up":
		m.treeCursor--
	case "enter":
		if m.treeCursor >= len(m.tree) {
			return
		}
		n := m.tree[m.treeCursor]
		if n.dir {
			m.open[n.path] = !m.open[n.path]
		} else {
			m.collapsed[n.path] = false
		}
		m.build()
		if !n.dir {
			m.cursor = m.header(n.path)
			m.offset = m.cursor // show the file from the top of the pane
		}
	}
}

// jumpUnread moves the cursor to the next unread thread, unfolding files as needed.
func (m *model) jumpUnread() {
	for _, t := range m.threads {
		if m.unread[t.ID] {
			m.collapsed[t.File] = false
		}
	}
	m.build()
	next := -1
	for i, r := range m.rows {
		if r.kind == rowThread && m.unread[r.thread] && (next < 0 || next <= m.cursor && i > m.cursor) {
			next = i
		}
	}
	if next < 0 {
		clear(m.unread) // their files left the diff
		return
	}
	m.focusTree = false
	m.cursor = next
}

// build regenerates rows and the visible tree from files, threads and fold state,
// keeping the cursors on the same file/line when possible.
func (m *model) build() {
	var at row
	if m.cursor < len(m.rows) {
		at = m.rows[m.cursor]
	}
	var treeAt string
	if m.treeCursor < len(m.tree) {
		treeAt = m.tree[m.treeCursor].path
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
			body = append(body, row{kind: rowHunk, file: f, frag: frag, old: old, new: new, text: frag.Header()})
			for i, l := range frag.Lines {
				r := row{kind: rowLine, file: f, frag: frag, idx: i, text: strings.TrimSuffix(l.Line, "\n")}
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
				m.rows = append(m.rows, threadRows(f, t, fmt.Sprintf("(outdated %s:%d) ", t.Side, t.Line))...)
			}
		}
		for _, r := range body {
			m.rows = append(m.rows, r)
			if r.kind == rowLine {
				for _, t := range attached[r.key()] {
					m.rows = append(m.rows, threadRows(f, t, "")...)
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
			n := node{path: strings.Join(parts[:d], "/"), name: parts[d-1], depth: d - 1, dir: d < len(parts)}
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
		i := slices.IndexFunc(m.rows, func(r row) bool {
			return path(r.file) == path(at.file) && r.kind == at.kind && r.old == at.old && r.new == at.new && r.thread == at.thread
		})
		if i < 0 {
			i = m.header(path(at.file))
		}
		if i >= 0 {
			m.cursor = i
		}
	}
	if i := slices.IndexFunc(m.tree, func(n node) bool { return n.path == treeAt }); i >= 0 {
		m.treeCursor = i
	}
	m.scroll()
}

func threadRows(f *gitdiff.File, t thread.Thread, label string) []row {
	var lines []string
	switch {
	case t.Resolved:
		lines = []string{fmt.Sprintf("💬 [resolved] %s%s (%d messages)", label, strings.SplitN(t.Messages[0].Body, "\n", 2)[0], len(t.Messages))}
	case t.Messages[len(t.Messages)-1].Author == "claude":
		lines = []string{"💬 [Claude replied] " + label}
	default:
		lines = []string{"💬 [waiting for Claude] " + label}
	}
	if !t.Resolved {
		for _, msg := range t.Messages {
			who := "you: "
			if msg.Author == "claude" {
				who = "claude: "
			}
			for i, l := range strings.Split(msg.Body, "\n") {
				if i > 0 {
					who = strings.Repeat(" ", len(who))
				}
				lines = append(lines, "  "+who+l)
			}
		}
	}
	rows := make([]row, len(lines))
	for i, l := range lines {
		rows[i] = row{kind: rowThread, file: f, thread: t.ID, text: l}
	}
	return rows
}

// header returns the row index of p's file header, or -1.
func (m *model) header(p string) int {
	return slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowFile && path(r.file) == p })
}

// scroll clamps cursors, keeps them on screen, marks the thread under the cursor read
// and syncs the tree with the diff cursor.
func (m *model) scroll() {
	h := m.bodyHeight()
	m.cursor = max(0, min(m.cursor, len(m.rows)-1))
	m.offset = max(0, min(m.offset, m.cursor), m.cursor-h+1)
	if m.cursor < len(m.rows) {
		r := m.rows[m.cursor]
		delete(m.unread, r.thread)
		if i := slices.IndexFunc(m.tree, func(n node) bool { return n.file == r.file }); i >= 0 && !m.focusTree {
			m.treeCursor = i
		}
	}
	m.treeCursor = max(0, min(m.treeCursor, len(m.tree)-1))
	m.treeOffset = max(0, min(m.treeOffset, m.treeCursor), m.treeCursor-h+1)
}

func (m model) bodyHeight() int {
	h := m.height - 1
	if m.editing {
		h -= lipgloss.Height(m.input.View())
	}
	if len(m.unread) > 0 {
		h--
	}
	return max(1, h)
}

func (m model) View() string {
	if m.width == 0 {
		return ""
	}
	h := m.bodyHeight()
	treeW := m.width * 3 / 10
	diffW := m.width - treeW - 1

	unreadFiles := map[string]bool{}
	for _, t := range m.threads {
		unreadFiles[t.File] = unreadFiles[t.File] || m.unread[t.ID]
	}
	var tree []string
	for i, n := range m.tree[m.treeOffset:min(len(m.tree), m.treeOffset+h)] {
		cur := m.treeOffset+i == m.treeCursor
		text := strings.Repeat("  ", n.depth)
		st := lipgloss.NewStyle()
		switch {
		case n.dir:
			arrow, icon := "▸ ", "\uf07b " // Nerd Font folder / folder-open
			if m.open[n.path] {
				arrow, icon = "▾ ", "\uf07c "
			}
			if !cur {
				icon = folderStyle.Render(icon) // inner colors would cut the cursor background
			}
			text += arrow + icon + n.name
		default:
			if unreadFiles[n.path] {
				text += "● "
			}
			add, del := stat(n.file)
			text += n.name + "  "
			if cur {
				text += fmt.Sprintf("+%d -%d", add, del) // inner colors would cut the cursor background
			} else {
				text += addStyle.Render(fmt.Sprintf("+%d", add)) + " " + delStyle.Render(fmt.Sprintf("-%d", del))
			}
			if m.cfg.Collapsed(n.path) {
				st = dimStyle
			}
		}
		if cur {
			st = treeCursorStyle
			if !m.focusTree {
				st = st.Foreground(lipgloss.Color("#1f2328")).Background(cursorBg)
			}
			text = "▌" + text
		} else {
			text = " " + text
		}
		tree = append(tree, st.Inline(true).Width(treeW).MaxWidth(treeW).Render(text))
	}

	var diff []string
	for i, r := range m.rows[m.offset:min(len(m.rows), m.offset+h)] {
		var text string
		st := lipgloss.NewStyle()
		switch r.kind {
		case rowFile:
			st = headerStyle
			name := path(r.file)
			if r.file.IsRename {
				name = r.file.OldName + " → " + r.file.NewName
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
			switch op {
			case gitdiff.OpAdd:
				st = addLineStyle
			case gitdiff.OpDelete:
				st = delLineStyle
			}
		case rowThread:
			st, text = threadStyle, strings.Repeat(" ", 10)+r.text
		}
		if m.offset+i == m.cursor && !m.focusTree {
			st = st.Bold(true).Background(cursorBg)
		}
		diff = append(diff, st.Inline(true).Width(diffW).MaxWidth(diffW).Render(strings.ReplaceAll(text, "\t", "    ")))
	}
	if len(m.rows) == 0 {
		diff = append(diff, dimStyle.Render("no changes against "+m.base))
	}

	var add, del int
	for _, f := range m.files {
		a, d := stat(f)
		add, del = add+a, del+d
	}
	footer := dimStyle.Render("○ Claude not connected") + "  "
	if len(m.peers) > 0 {
		footer = addStyle.Bold(true).Render(fmt.Sprintf("● Claude connected (%d)", len(m.peers))) + "  "
	}
	footer += fmt.Sprintf("%s ← %s  %d files  +%d -%d  │ ", m.branch, m.base, len(m.files), add, del)
	if m.err != nil {
		footer += errStyle.Render(m.err.Error())
	} else {
		footer += dimStyle.Render("tab focus  n/p file  x fold  c comment  r reply  R resolve  u unread  q quit")
	}

	var view string
	if len(m.unread) > 0 {
		view = bannerStyle.Width(m.width).Render(fmt.Sprintf("● Claude replied (%d) — press u to jump", len(m.unread))) + "\n"
	}
	view += lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(treeW).Height(h).Render(strings.Join(tree, "\n")),
		dimStyle.Render(strings.Repeat("│\n", h-1)+"│"),
		lipgloss.NewStyle().Width(diffW).Height(h).Render(strings.Join(diff, "\n")),
	)
	if m.editing {
		view += "\n" + m.input.View()
	}
	return view + "\n" + lipgloss.NewStyle().Inline(true).MaxWidth(m.width).Render(footer)
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
