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
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#6e7781"))
	threadStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7d4e00")).Background(lipgloss.Color("#fff8c5"))
	folderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#54aeff"))
	fileStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#59636e"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8c959f"))
	errStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#cf222e"))
	cursorBg     = lipgloss.Color("#b6e3ff")
	buttonStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#1f883d"))
	cancelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1f2328")).Background(lipgloss.Color("#eaeef2"))
	selStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#ffffff")).Background(lipgloss.Color("#0969da"))
	linkColor    = lipgloss.Color("#0969da")

	// urlRe finds URLs in screen text, which has no spaces or quotes in them and ends at the pane border.
	urlRe = regexp.MustCompile("https?://[^\\s<>\"'`│]+")

	sides = map[string]string{"old": "L", "new": "R"} // GitHub's left/right line notation
)

// chatID is the thread ID Claude replies to messages from the Message AI section with.
const chatID = "message"

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

// Messages from goroutines carry the worktree they are for, which need not be the one shown.
type (
	// peerMsg is a newly connected `di mcp` (one per Claude Code session); peerGoneMsg is one that disconnected.
	peerMsg struct {
		wt   *worktree
		conn net.Conn
	}
	peerGoneMsg struct {
		wt   *worktree
		conn net.Conn
	}
	// peerData is a message from a `di mcp`.
	peerData struct {
		wt *worktree
		thread.ToTUI
	}
	// pushedMsg is the result of Commit & Push.
	pushedMsg struct {
		wt  *worktree
		err error
	}
	// prMsg is the result of Open PR.
	prMsg struct {
		wt  *worktree
		err error
	}
	// openedMsg is the result of opening a clicked URL.
	openedMsg struct {
		wt  *worktree
		err error
	}
)

// worktree is the state of one worktree of the repo. di shows one at a time and serves the threads of all.
type worktree struct {
	repo   *git.Repo
	branch string
	raw    string
	files  []*gitdiff.File
	err    error

	threads     []thread.Thread
	seq         int               // last thread ID; IDs stay unique after a thread is resolved
	status      map[string]string // thread ID → Claude's latest set_status, until it replies
	restoreNote string            // feedback after the Restore button, e.g. " restored 2"
	chat        []thread.Message  // the Message AI conversation, not about a particular file or line
	chatUnread  bool              // Claude replied in chat while it was not shown
	peers       []net.Conn

	collapsed map[string]bool // file path → folded
	open      map[string]bool // dir path → expanded

	rows               []row
	tree               []node // visible nodes
	offset, treeOffset int    // first visible row / tree node
	current            string // file highlighted in the tree

	pushing bool
	pushErr error // Commit & Push / Open PR error, kept apart from err, which every reload overwrites
}

type model struct {
	*worktree             // the one shown
	wts       []*worktree // all served, in git's order

	slug  string
	cfg   config.RepoConfig
	base  string
	bases []string // diff targets selectable in the footer: the configured base, origin/main and HEAD (uncommitted changes)
	gen   int      // bumped on base or worktree change so in-flight loads for the old ones are dropped

	mouseX, mouseY int

	editing bool
	commit  bool // the textarea holds a commit message instead of a comment
	direct  bool // the textarea holds a message to Claude, in the Message AI section, instead of a comment

	chatOpen   bool      // the Message AI section under the diff is open
	chatScroll int       // chat lines scrolled up from its end
	wheelDir   int       // direction of the last wheel step taken: 1 down, -1 up
	wheelAt    time.Time // when it was taken
	replyTo    string    // thread ID being commented on; "" for a new thread on target
	target     row
	input      textarea.Model

	sel selection

	width, height int
}

// selection is the text being selected by dragging with the left button.
type selection struct {
	pressed, dragged bool // the button is down; it moved since the press
	x0, y0, x1, y1   int  // where it was pressed and where it is now
}

// Run serves each worktree of repo's repository that no other di serves, on its own socket, and shows repo first.
func Run(repo *git.Repo, slug string, cfg config.RepoConfig, base string) error {
	repos, err := repo.Worktrees()
	if err != nil {
		return err
	}
	input := textarea.New()
	input.SetStyles(textarea.DefaultLightStyles())
	input.SetVirtualCursor(false) // View places the real cursor, where the terminal draws IME preedit
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	input.ShowLineNumbers = false
	input.SetHeight(5)
	m := model{slug: slug, cfg: cfg, base: base, bases: slices.Compact([]string{base, "origin/main", "HEAD"}), input: input}
	var lns []net.Listener // lns[i] serves m.wts[i]
	defer func() {
		for _, ln := range lns {
			ln.Close() // also removes the socket file
		}
	}()
	for _, r := range repos {
		sock, err := thread.SocketPath(r.GitDir)
		if err != nil {
			return err
		}
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			if r.GitDir == repo.GitDir {
				return errors.New("di is already running in this worktree")
			}
			continue // another di serves it
		}
		os.Remove(sock) // stale socket from a crashed di
		ln, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		lns = append(lns, ln)
		m.wts = append(m.wts, &worktree{repo: r, branch: r.Branch(), collapsed: map[string]bool{}, open: map[string]bool{}, status: map[string]string{},
			seq: int(time.Now().Unix())}) // new IDs stay above those of threads restored from an earlier run
		if r.GitDir == repo.GitDir {
			m.worktree = m.wts[len(m.wts)-1]
		}
	}
	if m.worktree == nil {
		return fmt.Errorf("%s is not in git worktree list", repo.Root)
	}
	p := tea.NewProgram(m)
	for i, ln := range lns {
		wt := m.wts[i]
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				p.Send(peerMsg{wt, c})
				go func() {
					defer func() { c.Close(); p.Send(peerGoneMsg{wt, c}) }()
					for dec := json.NewDecoder(c); ; {
						var msg thread.ToTUI
						if dec.Decode(&msg) != nil {
							return
						}
						p.Send(peerData{wt, msg})
					}
				}()
			}
		}()
	}
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
		m.input.SetWidth(m.inputWidth())
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
		msg.wt.peers = append(msg.wt.peers, msg.conn)
		for _, t := range msg.wt.threads {
			if t.Messages[len(t.Messages)-1].Author == "user" {
				json.NewEncoder(msg.conn).Encode(thread.ToMCP{Thread: &t})
			}
		}
	case peerGoneMsg:
		msg.wt.peers = slices.DeleteFunc(msg.wt.peers, func(c net.Conn) bool { return c == msg.conn })
	case pushedMsg:
		msg.wt.pushing, msg.wt.pushErr = false, msg.err
		if msg.err == nil && msg.wt == m.worktree {
			m.gen++ // restarts the reload loop instead of starting a second one
			return m, m.load
		}
	case prMsg:
		msg.wt.pushErr = msg.err
	case openedMsg:
		if msg.err != nil {
			msg.wt.pushErr = msg.err
		}
	case peerData:
		wt := msg.wt
		if r := msg.Reply; r != nil && r.ID == chatID {
			wt.chat = append(wt.chat, thread.Message{Author: "claude", Body: r.Body})
			delete(wt.status, chatID)
			wt.chatUnread = wt.chatUnread || wt != m.worktree || !m.chatOpen
		} else if r != nil {
			if i := slices.IndexFunc(wt.threads, func(t thread.Thread) bool { return t.ID == r.ID }); i >= 0 {
				wt.threads[i].Messages = append(wt.threads[i].Messages, thread.Message{Author: "claude", Body: r.Body})
			}
			delete(wt.status, r.ID)
		}
		if s := msg.Status; s != nil && (s.ID == chatID || slices.ContainsFunc(wt.threads, func(t thread.Thread) bool { return t.ID == s.ID })) {
			wt.status[s.ID] = s.Text
		}
		if msg.Restore != nil {
			added := 0
			for _, t := range msg.Restore {
				if t.ID == chatID {
					if len(wt.chat) == 0 && len(t.Messages) > 0 {
						wt.chat = t.Messages
						added++
					}
					continue
				}
				n, _ := strconv.Atoi(t.ID)
				wt.seq = max(wt.seq, n)
				if len(t.Messages) > 0 && !slices.ContainsFunc(wt.threads, func(u thread.Thread) bool { return u.ID == t.ID }) {
					wt.threads = append(wt.threads, t)
					added++
				}
			}
			// Restored threads awaiting Claude don't show in the panel, so it says what happened.
			wt.restoreNote = fmt.Sprintf(" restored %d", added)
			if added == 0 {
				wt.restoreNote = " nothing to restore"
			}
		}
		if wt == m.worktree {
			m.build()
		}
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
		mouse := msg.Mouse()
		m.mouseX, m.mouseY = mouse.X, mouse.Y
		// A left press starts a selection; releasing it where it was pressed is a click, after a drag it copies the selection.
		switch msg.(type) {
		case tea.MouseClickMsg:
			if mouse.Button == tea.MouseLeft {
				m.sel = selection{pressed: true, x0: mouse.X, y0: mouse.Y, x1: mouse.X, y1: mouse.Y}
			}
		case tea.MouseMotionMsg:
			if m.sel.pressed && mouse.Button == tea.MouseLeft {
				m.sel.x1, m.sel.y1 = mouse.X, mouse.Y
				m.sel.dragged = m.sel.dragged || m.sel.x1 != m.sel.x0 || m.sel.y1 != m.sel.y0
			}
		case tea.MouseReleaseMsg:
			if !m.sel.pressed {
				break
			}
			m.sel.pressed = false
			if m.sel.dragged { // stays highlighted until the next press
				return m, tea.SetClipboard(m.selectedText())
			}
			m.sel = selection{}
			cmd := m.click(mouse.X, mouse.Y)
			return m, cmd
		case tea.MouseWheelMsg:
			m.sel = selection{} // the text under it moves
			// A line per event: the terminal already sends several per wheel notch (Ghostty 3) and one per
			// line of trackpad travel, so more would make trackpad scrolling jump.
			d := 1
			if mouse.Button == tea.MouseWheelUp {
				d = -1
			}
			// A trackpad moved slowly sends a lone event the other way now and then; a real turn
			// comes after a pause, so a reversal right after a step is dropped.
			now := time.Now()
			if d != m.wheelDir && now.Sub(m.wheelAt) < wheelTurn {
				break
			}
			m.wheelDir, m.wheelAt = d, now
			switch {
			case mouse.X < m.width*3/10:
				m.treeOffset += d
			case mouse.Y >= m.diffHeight():
				m.chatScroll -= d
			default:
				m.offset += d
			}
			m.scroll()
			if mouse.X >= m.width*3/10 && mouse.Y < m.diffHeight() && m.offset < len(m.rows) {
				m.current = path(m.rows[m.offset].file)
			}
		}
	}
	return m, nil
}

// wheelTurn is how soon after a wheel step one the other way counts as trackpad jitter.
const wheelTurn = 150 * time.Millisecond

// click handles a left click at (x, y): opening the URL under it, or pressing what is there.
func (m *model) click(x, y int) tea.Cmd {
	if m.commit { // the dialog is modal: only its buttons, " Commit " and " Cancel ", on its last line inside the border
		dx, dy, box := m.dialog()
		if y == dy+len(box)-2 {
			switch x -= dx + 2; {
			case x >= 0 && x < 8:
				return m.submit()
			case x >= 10 && x < 18:
				m.editing, m.commit = false, false
				m.input.Reset()
			}
		}
		return nil
	}
	// A click outside the open Message AI section takes the keys back from its input, so the arrows move
	// between files again; the draft stays for when the section is clicked.
	if m.direct && (x < m.width*3/10 || y < m.diffHeight()) {
		m.editing, m.direct = false, false
		m.input.Blur()
		m.scroll()
	}
	if u := m.urlAt(x, y); u != "" {
		return func() tea.Msg { return openedMsg{m.worktree, openURL(u)} }
	}
	treeW, h, wh, scr := m.width*3/10, m.bodyHeight(), m.wtHeight(), m.screen()
	inTree := x < treeW
	// The comment box's buttons, " Comment " at x 0-8 and " Cancel " at x 11-18 on its last line, indented to the code text.
	bx, by := treeW+1+codeX, slices.Index(scr, -1)+lipgloss.Height(m.input.View())
	switch {
	case y == m.height-1:
		// Base buttons in the footer, laid out as in View.
		fx := lipgloss.Width(m.connection() + m.branch + " ← ")
		for _, b := range m.bases {
			w := lipgloss.Width(" " + b + " ")
			if x >= fx && x < fx+w && b != m.base {
				m.base = b
				m.gen++
				return m.load
			}
			fx += w + 1
		}
		fx++ // two spaces before the commit button
		if !m.pushing && x >= fx && x < fx+lipgloss.Width(" Commit & Push ") {
			m.editing, m.commit, m.direct = true, true, false
			m.input.Placeholder = "Commit message (enter: commit & push, empty: timestamp)"
			m.input.SetWidth(m.inputWidth())
			m.scroll()
			return m.input.Focus()
		}
		fx += lipgloss.Width(map[bool]string{false: " Commit & Push ", true: " Pushing… "}[m.pushing]) + 2
		if x >= fx && x < fx+lipgloss.Width(" Open PR ") {
			return func() tea.Msg { return prMsg{m.worktree, m.repo.OpenPR()} }
		}
	case m.editing && !m.direct && y == by && slices.Contains(scr, -1):
		switch x := x - bx; {
		case x >= 0 && x < 9:
			return m.submit()
		case x >= 11 && x < 19:
			m.editing, m.commit, m.direct = false, false, false
			m.input.Reset()
			m.scroll()
		}
	case inTree && y < wh: // the Worktrees section: a title row, a row per worktree, then the Files title
		if y > 0 && y < wh-1 && m.wts[y-1] != m.worktree {
			m.worktree = m.wts[y-1]
			m.gen++ // drops the old worktree's loads, ending its reload loop
			m.editing, m.direct = false, false
			m.input.Reset()
			m.build()
			return m.load
		}
	case inTree && y < wh+m.treeHeight():
		if i := m.treeOffset + y - wh; i < len(m.tree) {
			n := m.tree[i]
			switch {
			case n.dir:
				m.open[n.path] = !m.open[n.path]
				m.build()
			case x < 3+2*n.depth: // on the file arrow (after the 1-cell cursor and the indent)
				m.collapsed[n.path] = !m.collapsed[n.path]
				m.build()
			default:
				m.jump(n.path)
			}
		}
	case inTree:
		// AI Replies panel: a title row, then 2 rows per thread with " Resolve " at x 0-8 on the first,
		// or " Restore " at x 0-8 when there are none.
		y, rs := y-wh-m.treeHeight()-1, m.replies()
		if len(rs) == 0 && y == 0 && x < 9 {
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
		if y%2 == 0 && x < 9 {
			m.threads = slices.DeleteFunc(m.threads, func(u thread.Thread) bool { return u.ID == t.ID })
			delete(m.status, t.ID)
			m.broadcast(thread.ToMCP{Resolved: t.ID})
			if m.editing && !m.commit && !m.direct && m.replyTo == t.ID {
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
	case y >= m.diffHeight() && y < h:
		return m.clickChat(x-treeW-1, y-m.diffHeight())
	case y < len(scr) && scr[y] >= 0:
		i := scr[y]
		switch r := m.rows[i]; r.kind {
		case rowFile:
			m.collapsed[path(r.file)] = !m.collapsed[path(r.file)]
			m.build()
		case rowComment, rowLine: // retargets an open textarea, keeping the draft
			m.editing, m.commit, m.direct, m.replyTo, m.target = true, false, false, r.thread, r
			m.chatOpen = false // the comment needs the room; the conversation stays
			m.input.Placeholder = "Leave a comment (enter: send, alt+enter: newline)"
			m.input.SetWidth(m.diffWidth() - codeX)
			// r is the anchor now: scroll the box below it into view.
			if over := i - m.offset + 2 + lipgloss.Height(m.input.View()) - m.diffHeight(); over > 0 {
				m.offset += over
			}
			m.scroll()
			return m.input.Focus()
		}
	}
	return nil
}

// clickChat handles a click at (x, y) in the Message AI section: its title opens or closes it, " Send " at x 2-7
// on its last line sends, and anywhere else brings the input there.
func (m *model) clickChat(x, y int) tea.Cmd {
	switch last := y == m.chatHeight()-1; {
	case y == 0:
		m.chatOpen, m.chatScroll = !m.chatOpen, 0
		if m.chatOpen {
			m.chatUnread = false
			return m.focusChat()
		}
		if m.direct {
			m.editing, m.direct = false, false
			m.input.Reset()
		}
		m.scroll()
	case m.direct && last && x >= 2 && x < 8:
		return m.submit()
	case !m.direct:
		return m.focusChat()
	}
	return nil
}

// focusChat moves the textarea to the open Message AI section, keeping the draft.
func (m *model) focusChat() tea.Cmd {
	m.editing, m.direct = true, true
	m.input.Placeholder = "Message Claude (enter: send, alt+enter: newline)"
	m.input.SetWidth(m.inputWidth())
	m.scroll()
	return m.input.Focus()
}

// submit sends the textarea content to Claude as a message in direct mode, commits and pushes with it as message
// in commit mode, and otherwise adds it to the thread being commented on, or starts a new one on target.
func (m *model) submit() tea.Cmd {
	if m.direct { // the input stays open for the next message
		body := strings.TrimSpace(m.input.Value())
		if body == "" {
			return nil
		}
		if len(m.peers) == 0 {
			m.pushErr = errors.New("Claude not connected") // the draft stays
			return nil
		}
		m.chat = append(m.chat, thread.Message{Author: "user", Body: body})
		m.chatScroll = 0
		m.broadcast(thread.ToMCP{Message: body})
		m.input.Reset()
		return nil
	}
	m.editing = false
	body := strings.TrimSpace(m.input.Value())
	m.input.Reset()
	if m.commit {
		m.commit, m.pushing, m.pushErr = false, true, nil
		m.scroll()
		return func() tea.Msg {
			return pushedMsg{m.worktree, m.repo.CommitPush(cmp.Or(body, strconv.FormatInt(time.Now().Unix(), 10)))}
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
func (w *worktree) broadcast(msg thread.ToMCP) {
	w.peers = slices.DeleteFunc(w.peers, func(c net.Conn) bool { return json.NewEncoder(c).Encode(msg) != nil })
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
	if !m.editing || m.commit || m.direct {
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
	if m.editing && !m.commit && !m.direct {
		n += lipgloss.Height(m.input.View()) + 1 // room for the inline box below the last row
	}
	m.offset = max(0, min(m.offset, n-m.diffHeight()))
	m.chatScroll = max(0, min(m.chatScroll, len(m.chatLog(m.diffWidth()))-m.chatLogHeight()))
	m.treeOffset = max(0, min(m.treeOffset, len(m.tree)-m.treeHeight()))
}

func (m model) bodyHeight() int { return max(1, m.height-1) }

// diffHeight is the diff's share of the right pane; the Message AI section under it takes the rest:
// its title row, or about half the pane when open.
func (m model) diffHeight() int {
	ch := 1
	if m.chatOpen {
		ch = max(m.bodyHeight()/2, lipgloss.Height(m.input.View())+4)
	}
	return max(1, m.bodyHeight()-ch)
}

func (m model) chatHeight() int { return m.bodyHeight() - m.diffHeight() }

// chatInput is the bottom of the open Message AI section: the textarea with the Send button under it,
// or a row standing for it while the textarea is elsewhere.
func (m model) chatInput(w int) []string {
	if !m.direct {
		return []string{"  " + cancelStyle.Inline(true).Width(w-2).MaxWidth(w-2).Render(" Message Claude…")}
	}
	var lines []string
	for _, l := range strings.Split(m.input.View(), "\n") {
		lines = append(lines, "  "+l)
	}
	return append(lines, "  "+buttonStyle.Render(" Send ")+dimStyle.Inline(true).MaxWidth(max(0, w-8)).Render("  → Claude, not about a particular line"))
}

// chatLogHeight is the lines of the open Message AI section between its title and its input.
func (m model) chatLogHeight() int {
	return max(0, m.chatHeight()-1-len(m.chatInput(m.diffWidth())))
}

// chatLog is the Message AI conversation wrapped to width w.
func (m model) chatLog(w int) []string {
	var lines []string
	for _, msg := range m.chat {
		who := "you: "
		if msg.Author == "claude" {
			who = "claude: "
		}
		for i, l := range strings.Split(ansi.Wrap(printable(msg.Body), max(1, w-2-len(who)), ""), "\n") {
			if i > 0 {
				who = strings.Repeat(" ", len(who))
			}
			lines = append(lines, "  "+dimStyle.Render(who)+links(lipgloss.NewStyle(), l))
		}
	}
	return lines
}

// chatView renders the Message AI section as lines of width w.
func (m model) chatView(w int) []string {
	title := "▶ Message AI"
	if m.chatOpen {
		title = "▼ Message AI"
	}
	switch {
	case m.status[chatID] != "":
		title += "  [AI: " + strings.ReplaceAll(printable(m.status[chatID]), "\n", " ") + "…]"
	case m.chatUnread:
		title += "  ● Claude replied"
	}
	lines := []string{headerStyle.Inline(true).Width(w).MaxWidth(w).Render(title)}
	if !m.chatOpen {
		return lines
	}
	log, n := m.chatLog(w), m.chatLogHeight()
	if len(log) == 0 {
		log = []string{dimStyle.Render("  Talk with Claude about anything, not tied to a line; it answers here.")}
	}
	end := max(0, len(log)-m.chatScroll)
	log = log[max(0, end-n):end]
	lines = append(lines, make([]string, n-len(log))...) // the conversation sits on the input, as in a chat
	lines = fit(append(append(lines, log...), m.chatInput(w)...), m.chatHeight())
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "") // a wider line would wrap and push the input down
	}
	return lines
}

// dialog is the commit dialog centered on the body: its position and lines.
func (m model) dialog() (x, y int, lines []string) {
	content := append([]string{lipgloss.NewStyle().Bold(true).Render("Commit & Push"), ""}, strings.Split(m.input.View(), "\n")...)
	content = append(content, "", buttonStyle.Render(" Commit ")+"  "+cancelStyle.Render(" Cancel ")+dimStyle.Render("  → commit all changes & push"))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#8c959f")).Padding(0, 1).Render(strings.Join(content, "\n"))
	lines = strings.Split(box, "\n")
	return max(0, (m.width-lipgloss.Width(box))/2), max(0, (m.bodyHeight()-len(lines))/2), lines
}

// screen maps each line of the diff pane to a row index, or -1 for the lines of the inline comment box,
// which follows its anchor: the target code line, or the Comment button of the thread being replied to.
func (m model) screen() []int {
	anchor := slices.IndexFunc(m.rows, func(r row) bool { return m.selected(r) && r.kind != rowThread })
	var s []int
	for i := m.offset; i < len(m.rows) && len(s) < m.diffHeight(); i++ {
		s = append(s, i)
		if i == anchor {
			s = append(s, slices.Repeat([]int{-1}, lipgloss.Height(m.input.View())+1)...)
		}
	}
	// The header of the file the top line belongs to sticks there, over that line, while its file scrolls under it.
	if len(s) > 0 && s[0] >= 0 && m.rows[s[0]].kind != rowFile {
		if h := m.header(path(m.rows[s[0]].file)); h >= 0 {
			s[0] = h
		}
	}
	return s[:min(len(s), m.diffHeight())]
}

// replies are the threads waiting on the user: Claude wrote the last message.
func (w *worktree) replies() []thread.Thread {
	return slices.DeleteFunc(slices.Clone(w.threads), func(t thread.Thread) bool {
		return t.Messages[len(t.Messages)-1].Author != "claude"
	})
}

// wtHeight is the Worktrees section's share of the top of the left pane, up to a third: a title row, a row
// per worktree, then the Files title that separates it from the tree. A single worktree has none.
func (m model) wtHeight() int {
	if len(m.wts) < 2 {
		return 0
	}
	return min(1+len(m.wts), m.bodyHeight()/3) + 1
}

// treeHeight is the tree's share of the left pane below the Worktrees section; the AI Replies panel takes the rest,
// up to half: a title row, then 2 rows per reply or the Restore row when there are none.
func (m model) treeHeight() int {
	h := m.bodyHeight() - m.wtHeight()
	return max(0, h-min(max(2, 1+2*len(m.replies())), h/2))
}

// inputWidth is the textarea's width in the current mode.
func (m model) inputWidth() int {
	switch {
	case m.commit:
		return min(m.width-6, 72)
	case m.direct:
		return m.diffWidth() - 2
	}
	return m.diffWidth() - codeX
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

	rs, wh, th := m.replies(), m.wtHeight(), m.treeHeight()
	var tree []string
	if wh > 0 {
		tree = append(tree, headerStyle.Inline(true).Width(treeW).MaxWidth(treeW).Render(" Worktrees"))
	}
	for _, w := range m.wts[:max(0, wh-2)] {
		// The branch, Claude's unanswered replies there and whether Claude is connected to it.
		name, mark, st, badge := printable(cmp.Or(w.branch, filepath.Base(w.repo.Root))), "○", dimStyle, ""
		if len(w.peers) > 0 {
			mark, st = "●", addStyle
		}
		if n := len(w.replies()) + map[bool]int{true: 1}[w.chatUnread]; n > 0 {
			badge = fmt.Sprintf(" ●%d", n)
		}
		if w == m.worktree { // plain: inner colors would cut the row background
			tree = append(tree, lipgloss.NewStyle().Foreground(lipgloss.Color("#1f2328")).Background(cursorBg).Inline(true).Width(treeW).MaxWidth(treeW).
				Render("▌"+mark+" "+name+badge))
			continue
		}
		tree = append(tree, lipgloss.NewStyle().Inline(true).MaxWidth(treeW).Render(" "+st.Render(mark)+" "+name+threadStyle.Render(badge)))
	}
	if wh > 0 {
		tree = append(tree, headerStyle.Inline(true).Width(treeW).MaxWidth(treeW).Render(" Files"))
	}
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
	tree = fit(tree, wh+th)
	tree = append(tree, headerStyle.Inline(true).Width(treeW).MaxWidth(treeW).Render(fmt.Sprintf(" AI Replies (%d)", len(rs))))
	if len(rs) == 0 {
		tree = append(tree, buttonStyle.Render(" Restore ")+dimStyle.Inline(true).MaxWidth(w).Render(cmp.Or(m.restoreNote, " ask Claude to restore threads")))
	}
	for _, t := range rs {
		msg, _, _ := strings.Cut(t.Messages[len(t.Messages)-1].Body, "\n")
		tree = append(tree,
			buttonStyle.Render(" Resolve ")+threadStyle.Inline(true).Width(w).MaxWidth(w).Render(printable(fmt.Sprintf(" %s %s%d", t.File, sides[t.Side], t.Line))),
			links(dimStyle, ansi.Truncate("  "+printable(msg), treeW, "")))
	}
	tree = fit(tree, h) // entries past the panel are not shown

	scr := m.screen()
	first := slices.Index(scr, -1) // the inline box's first line
	dh := m.diffHeight()
	// The textarea is in the commit dialog, at the bottom of the open Message AI section, or inline in the diff.
	var box []string
	switch {
	case m.direct:
		if v.Cursor = m.input.Cursor(); v.Cursor != nil {
			v.Cursor.X += treeW + 1 + 2
			v.Cursor.Y += h - lipgloss.Height(m.input.View()) - 1
		}
	case m.editing && !m.commit:
		k := m.target.key()
		t := thread.Thread{File: path(m.target.file), Side: k.side, Line: k.line}
		if i := slices.IndexFunc(m.threads, func(t thread.Thread) bool { return t.ID == m.replyTo }); i >= 0 {
			t = m.threads[i]
		}
		hint := printable(fmt.Sprintf("  → %s %s%d", t.File, sides[t.Side], t.Line))
		box = append(strings.Split(m.input.View(), "\n"), buttonStyle.Render(" Comment ")+"  "+cancelStyle.Render(" Cancel ")+dimStyle.Render(hint))
		if v.Cursor = m.input.Cursor(); v.Cursor != nil {
			if first < 0 || first+v.Cursor.Y >= dh { // the box is scrolled out
				v.Cursor = nil
			} else {
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
		text = ansi.Truncate(strings.ReplaceAll(text, "\t", "    "), w, "")
		diff = append(diff, st.Inline(true).Render(pre)+buttonStyle.Render(btn)+
			links(st, text+strings.Repeat(" ", max(0, w-ansi.StringWidth(text)))))
	}
	if len(m.rows) == 0 {
		diff = append(diff, dimStyle.Render("no changes against "+m.base))
	}
	diff = append(fit(diff, dh), m.chatView(diffW)...)

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
	v.Content = view + "\n" + lipgloss.NewStyle().Inline(true).MaxWidth(m.width).Render(footer)
	if spans := m.selSpans(); spans != nil {
		lines := strings.Split(v.Content, "\n")
		for y, sp := range spans {
			if y < len(lines) {
				l := lines[y]
				lines[y] = ansi.Truncate(l, sp[0], "") + selStyle.Render(ansi.Strip(ansi.Cut(l, sp[0], sp[1]))) + ansi.TruncateLeft(l, sp[1], "")
			}
		}
		v.Content = strings.Join(lines, "\n")
	}
	if m.commit {
		dx, dy, box := m.dialog()
		lines := strings.Split(v.Content, "\n")
		for i, b := range box {
			if y := dy + i; y < len(lines) {
				lines[y] = ansi.Truncate(lines[y], dx, "") + b + ansi.TruncateLeft(lines[y], dx+lipgloss.Width(b), "")
			}
		}
		v.Content = strings.Join(lines, "\n")
		if v.Cursor = m.input.Cursor(); v.Cursor != nil {
			v.Cursor.X += dx + 2 // border and padding
			v.Cursor.Y += dy + 3 // border, title and blank line
		}
	}
	return v
}

// selSpans maps each screen line the selection covers to its cells [l, r), or is nil without a selection.
// As in a terminal it runs from the earlier point to the later one, but within the pane it started in,
// and without the gutter and line numbers when it lies in code text.
func (m model) selSpans() map[int][2]int {
	s := m.sel
	if !s.dragged {
		return nil
	}
	treeW := m.width * 3 / 10
	l, r := 0, m.width
	switch {
	case s.y0 >= m.bodyHeight(): // the footer spans the screen
	case s.x0 < treeW:
		r = treeW
	default:
		l = treeW + 1
		if min(s.x0, s.x1) > l+codeX {
			l += codeX + 1 // after the +/- marker
		}
	}
	ax, ay, bx, by := s.x0, s.y0, s.x1, s.y1
	if by < ay || by == ay && bx < ax {
		ax, ay, bx, by = bx, by, ax, ay
	}
	spans := map[int][2]int{}
	for y := max(0, ay); y <= min(by, m.height-1); y++ {
		a, b := l, r
		if y == ay {
			a = max(l, min(ax, r))
		}
		if y == by {
			b = min(r, max(bx+1, l))
		}
		if a < b {
			spans[y] = [2]int{a, b}
		}
	}
	return spans
}

// selectedText is the selected text, each line without trailing spaces.
func (m model) selectedText() string {
	spans := m.selSpans()
	var out []string
	for y, l := range strings.Split(ansi.Strip(m.View().Content), "\n") {
		if sp, ok := spans[y]; ok {
			out = append(out, strings.TrimRight(ansi.Cut(l, sp[0], sp[1]), " "))
		}
	}
	return strings.Join(out, "\n")
}

// urlAt returns the URL shown at (x, y), or "". A URL running to the end of its line may be wrapped or cut
// on screen, so it is completed from the thread messages that contain it.
func (m model) urlAt(x, y int) string {
	lines := strings.Split(ansi.Strip(m.View().Content), "\n")
	if y < 0 || y >= len(lines) {
		return ""
	}
	l := lines[y]
	for _, loc := range urlRe.FindAllStringIndex(l, -1) {
		u := trimURL(l[loc[0]:loc[1]])
		if a := ansi.StringWidth(l[:loc[0]]); x < a || x >= a+ansi.StringWidth(u) {
			continue
		}
		if rest := strings.TrimLeft(l[loc[1]:], " "); rest == "" || strings.HasPrefix(rest, "│") {
			for _, t := range m.threads {
				for _, msg := range t.Messages {
					for _, full := range urlRe.FindAllString(msg.Body, -1) {
						if full = trimURL(full); len(full) > len(u) && strings.HasPrefix(full, u) {
							return full
						}
					}
				}
			}
		}
		return u
	}
	return ""
}

// trimURL drops the punctuation a URL found in text usually ends with, e.g. the period closing a sentence.
func trimURL(u string) string { return strings.TrimRight(u, ".,;:!?)]}'") }

// links renders text in st, with the URLs in it in the link color and underlined over the same background.
func links(st lipgloss.Style, text string) string {
	st = st.Inline(true)
	var b strings.Builder
	last := 0
	for _, loc := range urlRe.FindAllStringIndex(text, -1) {
		end := loc[0] + len(trimURL(text[loc[0]:loc[1]]))
		b.WriteString(st.Render(text[last:loc[0]]) + st.Foreground(linkColor).Underline(true).Render(text[loc[0]:end]))
		last = end
	}
	return b.String() + st.Render(text[last:])
}

// openURL opens u in the default browser.
func openURL(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Run()
}

// fit pads lines with empty ones, or cuts them, to n.
func fit(lines []string, n int) []string {
	n = max(0, n)
	if len(lines) >= n {
		return lines[:n]
	}
	return append(lines, make([]string, n-len(lines))...)
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
