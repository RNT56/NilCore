//go:build tui

// tui.go is the full-screen TUI (`nilcore tui`): the SAME conversational
// session.Session the chat REPL drives, under a Bubble Tea skin — a scrollback
// transcript, a live activity line (the braille spinner + cycling verb + streamed
// reasoning), an input box, and a status bar, with irreversible-action gates as a
// modal. It reuses buildChatSession + the boot helpers verbatim; only the
// reasoning sink (a tuiEmitter) and the approver (a modal tuiApprover) differ from
// the REPL.
//
// This is the ONLY file that imports the Charm stack, and it is an opt-in build
// (-tags tui) so the default binary stays dependency-free — invariant I6: the core
// (internal/) never imports Charm, only this presentation file does (the SQLite
// precedent for a sanctioned, isolated exception).
package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"nilcore/internal/emit"
	"nilcore/internal/session"
	"nilcore/internal/verb"
)

// tuiMain launches the TUI. It resolves the same boot context as chat, builds the
// one conversation Session wired to a tuiEmitter (reasoning sink) and a modal
// tuiApprover (gate), and runs the Bubble Tea program on the alt-screen.
func tuiMain(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	cf := chatFlags{
		common: registerCommon(fs),
		budget: fs.Float64("budget", chatDefaultBudget, "global dollar ceiling for the whole conversation"),
	}
	_ = fs.Parse(args)

	b := loadBoot(*cf.common.config)
	applyConfigDefaults(cf.common, b.cfg, flagsSet(fs))

	absDir := mustAbs(*cf.common.dir)
	log := openLog(*cf.common.logPath)
	defer log.Close()

	prov, err := resolveProvider(*cf.common.backendName, b)
	if err != nil {
		fatal(err)
	}
	if prov == nil {
		fatal(fmt.Errorf("nilcore tui requires the native backend (a model provider to route and converse with)"))
	}

	// The conversation ctx is created BEFORE the approver so the approver can select
	// on it: a quit/shutdown then unblocks any gate the drive is parked on, instead
	// of wedging the drive goroutine and hanging sess.Wait() forever.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gates := make(chan gateReq)
	em := newTUIEmitter()
	ap := &tuiApprover{ctx: ctx, gates: gates}

	sess, err := buildChatSession(chatDeps{
		flags:    cf,
		provider: prov,
		boot:     b,
		log:      log,
		baseRepo: absDir,
		emitter:  em,
		approver: ap,
	})
	if err != nil {
		fatal(err)
	}

	p := tea.NewProgram(newTUIModel(ctx, sess, prov.Model(), em, gates), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatal(err)
	}
	cancel()
	sess.Wait()
}

// tuiEmitter is the session's reasoning sink for the TUI. Like the serve sink it is
// an ORDERED, bounded, non-blocking queue: Emit appends and signals; the model
// drains the whole queue per wake. Under backpressure it coalesces by dropping the
// oldest TOKEN, NEVER a framed event — a dropped frame is a lost turn boundary that
// would merge two turns in the transcript. (The loop must never block on the UI, so
// the queue sheds rather than waits; tokens are coalescible, frames are not.)
type tuiEmitter struct {
	mu   sync.Mutex
	buf  []emit.Event
	wake chan struct{} // cap-1 edge: signals the model that buf changed
}

// tuiEmitBuffer bounds the queue; generous because the model drains every frame, so
// this only trips if the loop sprints far ahead of the render between frames.
const tuiEmitBuffer = 256

func newTUIEmitter() *tuiEmitter { return &tuiEmitter{wake: make(chan struct{}, 1)} }

func (e *tuiEmitter) Emit(ev emit.Event) {
	e.mu.Lock()
	e.buf = append(e.buf, ev)
	if len(e.buf) > tuiEmitBuffer {
		e.coalesce()
	}
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default: // a wake is already pending; the model drains the whole queue per wake
	}
}

// coalesce drops the oldest pending KindToken (never a frame) to stay bounded; see
// the serve sink's coalesce for the full rationale. Caller holds e.mu.
func (e *tuiEmitter) coalesce() {
	for i, ev := range e.buf {
		if ev.Kind == emit.KindToken {
			e.buf = append(e.buf[:i], e.buf[i+1:]...)
			return
		}
	}
	e.buf = e.buf[1:]
}

// drain removes and returns every pending event in order (nil if empty).
func (e *tuiEmitter) drain() []emit.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.buf) == 0 {
		return nil
	}
	out := e.buf
	e.buf = nil
	return out
}

// gateReq is one irreversible-action approval request routed to the modal.
type gateReq struct {
	action string
	reply  chan bool
}

// tuiApprover renders gates as a modal: Approve hands the request to the UI and
// blocks (the drive goroutine is meant to wait for the human) until the user
// answers. The gate IS a blocking decision — but BOTH the handoff and the wait
// select on a.ctx so a quit/shutdown can never wedge the drive goroutine (which
// would hang sess.Wait() forever and leak the goroutine). A cancelled ctx
// default-DENIES, honouring the same contract as the production approvers.
type tuiApprover struct {
	ctx   context.Context
	gates chan gateReq
}

func (a *tuiApprover) Approve(action string) bool {
	reply := make(chan bool, 1)
	select {
	case a.gates <- gateReq{action: action, reply: reply}:
	case <-a.ctx.Done():
		return false // torn down before the UI could pose the gate
	}
	select {
	case ans := <-reply:
		return ans
	case <-a.ctx.Done():
		return false // torn down while the modal was pending
	}
}

// --- model ---

type tuiModel struct {
	ctx     context.Context
	sess    *session.Session
	model   string // provider:model, for the header
	emitter *tuiEmitter
	gates   chan gateReq

	vp viewport.Model
	ta textarea.Model

	lines      []string        // committed transcript lines
	stream     strings.Builder // the in-progress streamed reasoning line (uncommitted)
	streamStep int             // loop step of the tokens currently in stream

	working bool
	start   time.Time
	tokens  int
	spin    verb.Spinner

	gate *gateReq // active gate modal (nil = none)

	width, height int
	ready         bool
}

func newTUIModel(ctx context.Context, sess *session.Session, model string, emitter *tuiEmitter, gates chan gateReq) tuiModel {
	ta := textarea.New()
	ta.Placeholder = "talk to the agent — it picks the machine and works while you type"
	ta.Prompt = "❯ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 8000
	ta.SetHeight(1)
	ta.Focus()
	return tuiModel{
		ctx: ctx, sess: sess, model: model, emitter: emitter, gates: gates,
		ta:   ta,
		spin: verb.New(1, verb.General),
	}
}

type (
	emitMsg      emit.Event   // a single event (direct injection in tests)
	emitBatchMsg []emit.Event // a drained batch from the emitter (the live path)
	gateMsg      gateReq
	tickMsg      time.Time
)

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(m.listenEvents(), m.listenGates(), m.tick(), textarea.Blink)
}

// listenEvents blocks until the emitter signals, then drains the WHOLE queue as one
// batch (so a single wake never strands later events). One listener is in flight at
// a time, re-armed after each batch is folded.
func (m tuiModel) listenEvents() tea.Cmd {
	return func() tea.Msg {
		<-m.emitter.wake
		return emitBatchMsg(m.emitter.drain())
	}
}
func (m tuiModel) listenGates() tea.Cmd {
	return func() tea.Msg { return gateMsg(<-m.gates) }
}
func (m tuiModel) tick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.ready = true
		return m, nil

	case tea.KeyMsg:
		return m.onKey(msg)

	case emitMsg:
		m.onEvent(emit.Event(msg))
		m.refresh()
		return m, m.listenEvents()

	case emitBatchMsg:
		for _, ev := range msg {
			m.onEvent(ev)
		}
		m.refresh()
		return m, m.listenEvents()

	case gateMsg:
		g := gateReq(msg)
		m.gate = &g
		return m, m.listenGates()

	case tickMsg:
		m.working = m.sess.PhaseNow() == session.Working
		if m.working && m.start.IsZero() {
			m.start = time.Time(msg)
		}
		if !m.working {
			m.start = time.Time{}
		}
		m.refresh()
		return m, m.tick()
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// onKey handles input: gate answers, controls, and message submission.
func (m tuiModel) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A gate modal captures keys until answered.
	if m.gate != nil {
		switch msg.String() {
		case "y", "Y", "enter", "left":
			m.gate.reply <- true
			m.append(styleOK.Render("  ✓ approved: ") + m.gate.action)
			m.gate = nil
		case "n", "N", "esc", "right":
			m.gate.reply <- false
			m.append(styleErr.Render("  ✗ denied: ") + m.gate.action)
			m.gate = nil
		case "ctrl+c":
			// ^C must escape a pending gate too (otherwise it is swallowed here and
			// the only way out is to answer). Deny it — the reply channel is buffered
			// so the send never blocks — then quit.
			m.gate.reply <- false
			m.gate = nil
			return m, tea.Quit
		}
		m.refresh()
		return m, nil
	}

	switch msg.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyEnter:
		line := strings.TrimSpace(m.ta.Value())
		m.ta.Reset()
		if line == "" {
			return m, nil
		}
		return m.submit(line)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// submit handles a typed line: local controls, or a Turn (queue/steer).
func (m tuiModel) submit(line string) (tea.Model, tea.Cmd) {
	switch strings.TrimSpace(line) {
	case "/quit", "/exit":
		return m, tea.Quit
	case "/cancel", "/stop":
		if m.sess.PhaseNow() == session.Working {
			m.append(styleDim.Render("  cancelling current run…"))
			go m.sess.Cancel()
		} else {
			m.append(styleDim.Render("  nothing running."))
		}
		m.refresh()
		return m, nil
	case "/help", "/?":
		m.append(styleDim.Render(tuiHelp))
		m.refresh()
		return m, nil
	}

	// Echo the principal's turn and its mode, then dispatch (Turn returns at once).
	m.append(styleYou.Render("❯ ") + line)
	if chatIsSteer(line) {
		m.append(styleWarn.Render("  steering — interrupting the current step…"))
	} else if m.sess.PhaseNow() == session.Working {
		m.append(styleDim.Render("  queued (delivered after this step)"))
	}
	go func() { _ = m.sess.Turn(m.ctx, line) }()
	m.refresh()
	return m, nil
}

// onEvent folds one emit event into the transcript: tokens stream into the live
// line; a framed event commits the stream and appends its own glyph line.
func (m *tuiModel) onEvent(ev emit.Event) {
	switch ev.Kind {
	case emit.KindToken:
		// A token whose step differs from the open stream's is a new turn whose framed
		// boundary was (defensively) lost: commit the prior line first so two turns
		// never merge. Frames are never coalesced away, so this is belt-and-suspenders.
		if m.stream.Len() > 0 && ev.Step != m.streamStep {
			m.commitStream()
		}
		if m.stream.Len() == 0 {
			m.streamStep = ev.Step
		}
		m.stream.WriteString(ev.Text)
		m.tokens += len(ev.Text) / 4
	case emit.KindIntent:
		m.commitStream()
		m.append(styleDim.Render("  · " + ev.Text))
	case emit.KindTool:
		m.commitStream()
		m.append("  " + styleInfo.Render("▸") + " " + ev.Text)
	case emit.KindVerify:
		m.commitStream()
		glyph := styleOK.Render("✓")
		if isVerifyFailure(ev.Text) {
			glyph = styleErr.Render("✗")
		}
		m.append("  " + glyph + " " + ev.Text)
	case emit.KindSteerAck:
		m.commitStream()
		m.append("  " + styleWarn.Render("⤺ "+ev.Text))
	default:
		m.commitStream()
		m.append("  " + ev.Text)
	}
}

func (m *tuiModel) commitStream() {
	if m.stream.Len() > 0 {
		m.append("  " + styleStream.Render(strings.TrimRight(m.stream.String(), "\n")))
		m.stream.Reset()
	}
}

func (m *tuiModel) append(line string) { m.lines = append(m.lines, line) }

// refresh re-renders the transcript (including the in-progress stream) into the
// viewport and pins it to the bottom.
func (m *tuiModel) refresh() {
	body := strings.Join(m.lines, "\n")
	if m.stream.Len() > 0 {
		live := "  " + styleStream.Render(m.stream.String()) + styleInfo.Render("▌")
		if body != "" {
			body += "\n"
		}
		body += live
	}
	m.vp.SetContent(body)
	m.vp.GotoBottom()
}

func (m *tuiModel) layout() {
	headerH, statusH, inputH := 1, 1, 3
	vpH := m.height - headerH - statusH - inputH
	if vpH < 3 {
		vpH = 3
	}
	if !m.ready {
		m.vp = viewport.New(m.width, vpH)
	} else {
		m.vp.Width, m.vp.Height = m.width, vpH
	}
	m.ta.SetWidth(m.width - 2)
}

func (m tuiModel) View() string {
	if !m.ready {
		return "starting nilcore tui…"
	}
	if m.gate != nil {
		return m.viewGate()
	}
	return strings.Join([]string{m.header(), m.vp.View(), m.activity(), m.ta.View(), m.status()}, "\n")
}

func (m tuiModel) header() string {
	left := styleBrand.Render("◆ nilcore") + styleDim.Render(" · tui · "+m.model)
	return styleHeader.Width(m.width).Render(left)
}

// activity is the live line: the braille spinner + cycling verb + elapsed + a
// running token estimate while the agent works; blank when idle.
func (m tuiModel) activity() string {
	if !m.working || m.start.IsZero() {
		return strings.Repeat(" ", 0)
	}
	d := time.Since(m.start)
	meta := humanDur(d)
	if m.tokens > 0 {
		meta += fmt.Sprintf(" · %dk tok", m.tokens/1000)
		if m.tokens < 1000 {
			meta = humanDur(d) + fmt.Sprintf(" · %d tok", m.tokens)
		}
	}
	return styleWarn.Render(m.spin.Frame(d)+" "+m.spin.Verb(d)+"…") +
		styleDim.Render("  "+meta+" · ") + styleWarn.Render("! to steer")
}

func (m tuiModel) status() string {
	phase := strings.ToLower(m.sess.PhaseNow().String())
	tag := styleTag.Render(" " + strings.ToUpper(phase) + " ")
	hints := styleDim.Render("enter send · ! steer · /cancel · /quit · ^C exit")
	gap := m.width - lipgloss.Width(tag) - lipgloss.Width(hints) - 1
	if gap < 1 {
		gap = 1
	}
	return styleStatus.Width(m.width).Render(tag + strings.Repeat(" ", gap) + hints)
}

func (m tuiModel) viewGate() string {
	box := styleGate.Render(
		styleWarn.Render("GATE — irreversible action") + "\n\n" +
			m.gate.action + "\n\n" +
			styleOK.Render("[y] approve") + "    " + styleErr.Render("[n] deny"))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}

func humanDur(d time.Duration) string {
	s := int(d.Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

func isVerifyFailure(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "did not pass") || strings.Contains(l, "not verified") || strings.Contains(l, "failed")
}

const tuiHelp = `  talk to route a quick fix, a feature, or a whole project — it decides.
  plain text          queue (folds in at the next step)
  !text  /steer       steer (interrupt the current step, fold your feedback)
  /cancel  /stop      abort the current run, stay in the conversation
  /quit   ^C          leave`

// --- styles ---

var (
	styleBrand  = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleHeader = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Padding(0, 1)
	styleStatus = lipgloss.NewStyle().Padding(0, 1)
	styleTag    = lipgloss.NewStyle().Background(lipgloss.Color("3")).Foreground(lipgloss.Color("0")).Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleYou    = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	styleInfo   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleStream = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	styleGate   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("11")).Padding(1, 3)
)
