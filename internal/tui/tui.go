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
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#1f2328")).Background(lipgloss.Color("#eaeef2"))
	threadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7d4e00")).Background(lipgloss.Color("#fff8c5"))
	folderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#54aeff"))
	fileStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#59636e"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8c959f"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	cursorBg     = lipgloss.Color("#b6e3ff")
	buttonStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#1f883d"))
	cancelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1f2328")).Background(lipgloss.Color("#eaeef2"))
)

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

type model struct {
	repo  *git.Repo
	slug  string
	cfg   config.RepoConfig
	base  string
	bases [2]string // diff targets selectable in the footer: the configured base and HEAD (uncommitted changes)
	gen   int       // bumped on base change so in-flight loads for the old base are dropped

	branch string
	raw    string
	files  []*gitdiff.File
	err    error

	threads []thread.Thread
	peers   []net.Conn

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
	pushErr error // kept apart from err, which every reload overwrites

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

	// Light theme only. Also keeps adaptive colors (textarea) from querying the terminal mid-session,
	// which stalls and leaks the reply into the input.
	lipgloss.SetHasDarkBackground(false)
	input := textarea.New()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	input.ShowLineNumbers = false
	input.SetHeight(5)
	p := tea.NewProgram(model{repo: repo, slug: slug, cfg: cfg, base: base, bases: [2]string{base, "HEAD"}, input: input,
		collapsed: map[string]bool{}, open: map[string]bool{}}, tea.WithAltScreen(), tea.WithMouseAllMotion())
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
	return loadedMsg{cfg: cfg, branch: m.repo.Branch(), raw: raw, files: files, err: err, gen: m.gen}
}

func (m model) Init() tea.Cmd { return m.load }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width)
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
				json.NewEncoder(msg.conn).Encode(t)
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
	case thread.Reply:
		i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == msg.ID })
		if i < 0 {
			return m, nil
		}
		m.threads[i].Messages = append(m.threads[i].Messages, thread.Message{Author: "claude", Body: msg.Body})
		m.build()
	case tea.KeyMsg:
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
		m.mouseX, m.mouseY = msg.X, msg.Y
		treeW, h := m.width*3/10, m.bodyHeight()
		inTree := msg.X < treeW
		switch {
		case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
			d := 3
			if msg.Button == tea.MouseButtonWheelUp {
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
		case msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft:
		case msg.Y == m.height-1:
			// Base buttons in the footer, laid out as in View.
			x := lipgloss.Width(m.status() + m.branch + " ← ")
			for _, b := range m.bases {
				w := lipgloss.Width(" " + b + " ")
				if msg.X >= x && msg.X < x+w && b != m.base {
					m.base = b
					m.gen++
					return m, m.load
				}
				x += w + 1
			}
			x++ // two spaces before the commit button
			if !m.pushing && msg.X >= x && msg.X < x+lipgloss.Width(" Commit & Push ") {
				m.editing, m.commit = true, true
				m.input.Placeholder = "Commit message (enter: commit & push, empty: timestamp)"
				m.scroll()
				return m, m.input.Focus()
			}
		case msg.Y >= h:
			// Buttons under the textarea: " Comment " at x 0-8, " Cancel " at x 11-18.
			if m.editing && msg.Y == h+lipgloss.Height(m.input.View()) {
				switch {
				case msg.X < 9:
					cmd := m.submit()
					return m, cmd
				case msg.X >= 11 && msg.X < 19:
					m.editing, m.commit = false, false
					m.input.Reset()
					m.scroll()
				}
			}
		case inTree:
			if i := m.treeOffset + msg.Y; i < len(m.tree) {
				n := m.tree[i]
				if n.dir {
					m.open[n.path] = !m.open[n.path]
					m.build()
				} else {
					m.jump(n.path)
				}
			}
		default:
			if i := m.offset + msg.Y; i < len(m.rows) {
				switch r := m.rows[i]; {
				case r.kind == rowFile:
					m.collapsed[path(r.file)] = !m.collapsed[path(r.file)]
					m.build()
				case r.kind == rowComment || r.kind == rowLine: // retargets an open textarea, keeping the draft
					m.editing, m.commit, m.replyTo, m.target = true, false, r.thread, r
					m.input.Placeholder = "Leave a comment (enter: send, alt+enter: newline)"
					m.scroll()
					return m, m.input.Focus()
				}
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
		m.broadcast(m.threads[i])
	} else {
		r := m.target
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
	return nil
}

// broadcast sends t to every connected `di mcp`, dropping peers that went away.
func (m *model) broadcast(t thread.Thread) {
	m.peers = slices.DeleteFunc(m.peers, func(c net.Conn) bool { return json.NewEncoder(c).Encode(t) != nil })
}

// build regenerates rows and the visible tree from files, threads and fold state,
// keeping the same file/line at the top of the diff when possible.
func (m *model) build() {
	var at row
	if m.offset < len(m.rows) {
		at = m.rows[m.offset]
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
				m.rows = append(m.rows, threadRows(f, t, fmt.Sprintf("(outdated %s:%d) ", t.Side, t.Line), m.diffWidth()-12)...)
			}
		}
		for _, r := range body {
			m.rows = append(m.rows, r)
			if r.kind == rowLine {
				for _, t := range attached[r.key()] {
					m.rows = append(m.rows, threadRows(f, t, "", m.diffWidth()-12)...)
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
			m.offset = i
		}
	}
	m.scroll()
}

// threadRows renders t as rows of at most width cells, wrapping message bodies.
func threadRows(f *gitdiff.File, t thread.Thread, label string, width int) []row {
	var lines []string
	switch {
	case t.Messages[len(t.Messages)-1].Author == "claude":
		lines = []string{"💬 [Claude replied] " + label}
	default:
		lines = []string{"💬 [waiting for Claude] " + label}
	}
	for _, msg := range t.Messages {
		who := "you: "
		if msg.Author == "claude" {
			who = "claude: "
		}
		for i, l := range strings.Split(ansi.Wrap(msg.Body, width-2-len(who), ""), "\n") {
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
		h := m.bodyHeight()
		m.treeOffset = max(min(m.treeOffset, i), i-h+1)
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
	h := m.bodyHeight()
	m.offset = max(0, min(m.offset, len(m.rows)-h))
	m.treeOffset = max(0, min(m.treeOffset, len(m.tree)-h))
}

func (m model) bodyHeight() int {
	h := m.height - 1
	if m.editing {
		h -= lipgloss.Height(m.input.View()) + 1 // + buttons
	}
	return max(1, h)
}

func (m model) diffWidth() int { return m.width - m.width*3/10 - 1 }

func (m model) View() string {
	if m.width == 0 {
		return ""
	}
	h := m.bodyHeight()
	treeW := m.width * 3 / 10
	diffW := m.diffWidth()

	var tree []string
	for _, n := range m.tree[m.treeOffset:min(len(m.tree), m.treeOffset+h)] {
		cur := !n.dir && n.path == m.current
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
			icon := "  \uf016 " // Nerd Font file, indented past the dir arrow to align with sibling dirs
			if !cur {
				icon = fileStyle.Render(icon)
			}
			add, del := stat(n.file)
			text += icon + n.name + "  "
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
			st = st.Foreground(lipgloss.Color("#1f2328")).Background(cursorBg)
			text = "▌" + text
		} else {
			text = " " + text
		}
		tree = append(tree, st.Inline(true).Width(treeW).MaxWidth(treeW).Render(text))
	}

	hover := -1
	if m.mouseX >= treeW && m.mouseY < h {
		hover = m.offset + m.mouseY
	}
	var diff []string
	for i, r := range m.rows[m.offset:min(len(m.rows), m.offset+h)] {
		var text string
		st := lipgloss.NewStyle()
		pre, btn := "  ", "" // gutter, button after it
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
			if m.offset+i == hover || m.selected(r) {
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
	footer := m.status() + m.branch + " ←"
	for _, b := range m.bases {
		st := cancelStyle
		if b == m.base {
			st = buttonStyle
		}
		footer += " " + st.Render(" "+b+" ")
	}
	footer += "  " + buttonStyle.Render(map[bool]string{false: " Commit & Push ", true: " Pushing… "}[m.pushing])
	footer += fmt.Sprintf("  %d files  +%d -%d", len(m.files), add, del)
	if m.err != nil {
		footer += "  │ " + errStyle.Render(m.err.Error())
	}
	if m.pushErr != nil {
		footer += "  │ " + errStyle.Render(m.pushErr.Error())
	}

	view := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(treeW).Height(h).Render(strings.Join(tree, "\n")),
		dimStyle.Render(strings.Repeat("│\n", h-1)+"│"),
		lipgloss.NewStyle().Width(diffW).Height(h).Render(strings.Join(diff, "\n")),
	)
	if m.editing {
		btn, hint := " Commit ", "  → commit all changes & push"
		if !m.commit {
			k := m.target.key()
			t := thread.Thread{File: path(m.target.file), Side: k.side, Line: k.line}
			if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == m.replyTo }); i >= 0 {
				t = m.threads[i]
			}
			side := map[string]string{"old": "L", "new": "R"}[t.Side] // GitHub's left/right line notation
			btn, hint = " Comment ", fmt.Sprintf("  → %s %s%d", t.File, side, t.Line)
		}
		view += "\n" + m.input.View() + "\n" + buttonStyle.Render(btn) + "  " + cancelStyle.Render(" Cancel ") + dimStyle.Render(hint)
	}
	return view + "\n" + lipgloss.NewStyle().Inline(true).MaxWidth(m.width).Render(footer)
}

// status is the footer's leading Claude connection indicator.
func (m model) status() string {
	if len(m.peers) > 0 {
		return addStyle.Bold(true).Render(fmt.Sprintf("● Claude connected (%d)", len(m.peers))) + "  "
	}
	return dimStyle.Render("○ Claude not connected") + "  "
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
