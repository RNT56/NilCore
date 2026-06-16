package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"nilcore/internal/budget"
	"nilcore/internal/eventlog"
	"nilcore/internal/inbox"
	"nilcore/internal/model"
	"nilcore/internal/policy"
	"nilcore/internal/sandbox"
	"nilcore/internal/session"
	"nilcore/internal/termui"
	"nilcore/internal/verb"
)

// chat_test.go is the hermetic test of the `nilcore chat` REPL wiring (C3-T01). It
// drives NO container, NO network, and NO live model: it tests the input parse
// (queue-vs-steer routing), the REPL→Session wiring (each typed line becomes a
// Turn; a plain line queues and a '!' line steers an in-flight drive), clean
// shutdown, and the session construction (router + four drivers, conversation-keyed
// budget). The interactive loop is exercised against a real session.Session backed
// by a fake blocking driver, so the queue/steer classification is the production
// path, not a re-implementation.

// --- input parse: queue vs steer -------------------------------------------

func TestParseChatLine(t *testing.T) {
	cases := []struct {
		in      string
		cmd     string
		handled bool
	}{
		{"/quit", "quit", true},
		{"/exit", "quit", true},
		{"  /quit  ", "quit", true}, // surrounding space tolerated
		{"/help", "help", true},
		{"/?", "help", true},
		// The shared verbs (/status /mode /cancel /clear /add /discuss …) are handled
		// by session.ParseControl now, NOT parseChatLine — so they read as unhandled here.
		{"/status", "", false},
		{"/mode", "", false},
		{"/cancel", "", false},
		{"fix the failing test", "", false}, // ordinary message
		{"!the path is wrong", "", false},   // a steer is NOT a control verb
		{"/steer use ./service", "", false}, // /steer flows to Turn, not handled here
		{"/plan", "", false},                // mode verbs are handled by ParseControl, not here
		{"", "", false},                     // blank line
	}
	for _, c := range cases {
		cmd, handled := parseChatLine(c.in)
		if cmd != c.cmd || handled != c.handled {
			t.Errorf("parseChatLine(%q) = (%q,%v), want (%q,%v)", c.in, cmd, handled, c.cmd, c.handled)
		}
	}
}

// TestChatIsSteerMatchesSession asserts the REPL's steer-detection (used for the
// terminal ack) agrees with what the Session's own classifier will actually do, so
// the printed "queued"/"steering" line never drifts from the real mode. It compares
// chatIsSteer against the inbox.Mode the session pushes for the same line.
func TestChatIsSteerMatchesSession(t *testing.T) {
	cases := []struct {
		line  string
		steer bool
	}{
		{"plain message", false},
		{"  leading space queues", false},
		{"!steer now", true},
		{"  !steer with space", true},
		{"/steer correct the path", true},
		{"/steer", true},
		{"/steering is not steer", false}, // only exact "/steer" or "/steer " prefix
	}
	for _, c := range cases {
		if got := chatIsSteer(c.line); got != c.steer {
			t.Errorf("chatIsSteer(%q) = %v, want %v", c.line, got, c.steer)
		}
		// Cross-check against the session's classifier via a live push: a real
		// Session pushes Steer iff the line is a steer.
		mode := sessionPushMode(t, c.line)
		wantMode := inbox.Queue
		if c.steer {
			wantMode = inbox.Steer
		}
		if mode != wantMode {
			t.Errorf("session classified %q as %v, want %v (REPL ack would drift)", c.line, mode, wantMode)
		}
	}
}

// sessionPushMode drives one mid-work Turn through a real Session (held in Working
// by a blocking driver) and reports whether the line fired the steer signal — i.e.
// the inbox.Mode the production classifier assigned. It joins the drive cleanly.
func sessionPushMode(t *testing.T, line string) inbox.Mode {
	t.Helper()
	d := newBlockingDriver()
	sess := session.New("probe", "local", t.TempDir(), nil)
	sess.Router = staticRouter{session.RouteNative}
	sess.Drivers = session.Drivers{Native: d}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start a drive (this first Turn routes; the driver blocks, holding Working).
	if err := sess.Turn(ctx, "start the work"); err != nil {
		t.Fatalf("start Turn: %v", err)
	}
	waitPhase(t, sess, session.Working)

	// The mid-work line: queued or steered.
	if err := sess.Turn(ctx, line); err != nil {
		t.Fatalf("mid-work Turn: %v", err)
	}
	mode := inbox.Queue
	select {
	case <-sess.Inbox.Steer():
		mode = inbox.Steer
	case <-time.After(50 * time.Millisecond):
	}

	d.release()
	cancel()
	sess.Wait()
	return mode
}

// --- REPL → Session wiring: queue vs steer end-to-end -----------------------

// TestChatREPLQueuesAndSteers drives the real chatREPL against a real Session with
// a fake blocking driver and asserts the production behavior: the first line routes
// a drive, a plain mid-work line lands in the Inbox WITHOUT firing the steer signal
// (queue), and a '!' line fires it (steer). It then EOFs and asserts a clean return
// with no leaked goroutine.
func TestChatREPLQueuesAndSteers(t *testing.T) {
	base := waitGoroutines(runtime.NumGoroutine())

	d := newBlockingDriver()
	sess := session.New(chatConvoID, chatPrincipal, t.TempDir(), nil)
	sess.Router = staticRouter{session.RouteNative}
	sess.Drivers = session.Drivers{Native: d}

	// A scripted stdin: start a drive, queue one line, steer one line, then EOF.
	// blockReader releases each line only when the test pumps it, so we can observe
	// the drive reach Working before the follow-ups arrive.
	r := newScriptReader("start the work", "also add a flag", "!the path is ./service")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- chatREPL(ctx, sess, r, termui.New(&out), nil) }()

	// Line 1 routes a drive; wait for Working.
	r.next()
	waitPhase(t, sess, session.Working)

	// Line 2 (plain) → QUEUE: it lands in the inbox but does NOT fire steer.
	r.next()
	if !waitInboxLen(t, sess, 1) {
		t.Fatal("queued line never reached the inbox")
	}
	select {
	case <-sess.Inbox.Steer():
		t.Fatal("a plain line fired the steer signal (should queue)")
	default:
	}

	// Line 3 ('!') → STEER: it fires the steer signal.
	r.next()
	if !waitSteer(t, sess) {
		t.Fatal("a '!' line did not fire the steer signal (should steer)")
	}

	// EOF: the reader scans to completion; chatREPL returns nil on clean EOF.
	r.close()
	d.release()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("chatREPL returned %v, want clean EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chatREPL did not return after EOF")
	}

	cancel()
	sess.Wait()

	// The ack lines must have been surfaced so the user knew the mode.
	if s := out.String(); !strings.Contains(s, "queued") || !strings.Contains(s, "steering") {
		t.Errorf("REPL did not ack both modes; output:\n%s", s)
	}

	if g := waitGoroutines(base); g > base {
		t.Errorf("goroutine leak: started %d, ended %d", base, g)
	}
}

// TestChatREPLShutsDownOnCtxCancel asserts Ctrl-C (ctx cancel) makes chatREPL
// return PROMPTLY — the clean-shutdown rail — even while the reader is parked on a
// blocking read. (The detached stdin reader goroutine cannot be reaped while it is
// blocked inside a raw Read with no EOF; that is inherent to a line reader over a
// real terminal and the process exits around it. The no-leak guarantee is asserted
// in the EOF path, TestChatREPLQueuesAndSteers, where the reader genuinely exits.)
func TestChatREPLShutsDownOnCtxCancel(t *testing.T) {
	sess := session.New(chatConvoID, chatPrincipal, t.TempDir(), nil)
	sess.Router = staticRouter{session.RouteNative}
	sess.Drivers = session.Drivers{Native: newBlockingDriver()}

	// A reader that blocks until released, then returns EOF — modeling a terminal
	// awaiting input. We never release it: only the ctx cancel must unblock the REPL.
	r := newScriptReader() // no lines; Read blocks on the pump channel

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- chatREPL(ctx, sess, r, termui.New(io.Discard), nil) }()

	// Cancel (Ctrl-C) and require a prompt return.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("chatREPL on cancel returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chatREPL did not return on ctx cancel")
	}
	sess.Wait()
	r.close() // let the detached reader goroutine observe EOF and exit cleanly
}

// TestChatREPLStatusAndQuit asserts the local control verbs work without touching a
// drive: /status reports the phase and /quit returns cleanly.
func TestChatREPLStatusAndQuit(t *testing.T) {
	sess := session.New(chatConvoID, chatPrincipal, t.TempDir(), nil)
	sess.Router = staticRouter{session.RouteNative}
	sess.Drivers = session.Drivers{Native: newBlockingDriver()}

	r := newScriptReader("/status", "/quit")
	var out strings.Builder
	done := make(chan error, 1)
	go func() { done <- chatREPL(context.Background(), sess, r, termui.New(&out), nil) }()

	r.next() // /status — reads phase, returns to prompt
	r.next() // /quit — returns nil

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("chatREPL returned %v on /quit, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chatREPL did not return on /quit")
	}
	if s := out.String(); !strings.Contains(s, "idle") {
		t.Errorf("/status did not report the idle phase; output:\n%s", s)
	}
}

// --- session construction: router + four drivers, conversation budget -------

// TestBuildChatSessionWiring asserts buildChatSession assembles a Session wired
// with the router and all four drivers, and that the budget is a single
// conversation-keyed wall: a charge past the ceiling under the conversation id
// returns budget.ErrCeiling (so N follow-ups share ONE ceiling, never N×).
func TestBuildChatSessionWiring(t *testing.T) {
	repo := t.TempDir()
	log := newMemLog(t)
	flags := newChatFlags(repo)
	budgetCeiling := 0.001 // a tiny ceiling so one priced call trips it
	*flags.budget = budgetCeiling

	prov := &chatFakeProvider{id: "claude-sonnet-4-6", usage: model.Usage{InputTokens: 100, OutputTokens: 100}}
	sess, err := buildChatSession(chatDeps{
		flags:    flags,
		provider: prov,
		boot:     boot{cred: func(string) string { return "" }},
		log:      log,
		baseRepo: repo,
		emitter:  termui.NewEmitter(termui.New(io.Discard), verb.General),
	})
	if err != nil {
		t.Fatalf("buildChatSession: %v", err)
	}

	// The conversational identity is pinned to the local principal + conversation id.
	if sess.ID != chatConvoID {
		t.Errorf("session ID = %q, want %q", sess.ID, chatConvoID)
	}
	if sess.Sender != chatPrincipal {
		t.Errorf("session Sender = %q, want %q", sess.Sender, chatPrincipal)
	}
	if sess.Repo != repo {
		t.Errorf("session Repo = %q, want %q", sess.Repo, repo)
	}

	// Router + all four drivers must be wired (every Route has a machine).
	if sess.Router == nil {
		t.Error("router not wired")
	}
	if sess.Drivers.Native == nil || sess.Drivers.Supervise == nil ||
		sess.Drivers.Project == nil || sess.Drivers.Chat == nil {
		t.Errorf("not all drivers wired: %+v", sess.Drivers)
	}
	if sess.Out == nil {
		t.Error("emitter not wired (reasoning would not stream to stdout)")
	}
	if sess.Budget == nil {
		t.Fatal("conversation budget ledger not wired")
	}

	// The budget is a real conversation-keyed wall: charge under the conversation id
	// past the ceiling → ErrCeiling. This is the §6 property: the conversation id is
	// the budget key, so the ceiling is shared across drives.
	err = sess.Budget.Charge(context.Background(), chatConvoID, 0, budgetCeiling*2)
	if !errors.Is(err, budget.ErrCeiling) {
		t.Errorf("charge past the conversation ceiling = %v, want ErrCeiling", err)
	}
}

// TestChatShouldSupervise covers the native-vs-supervise sizing heuristic the
// router reconciles against: a small localized ask stays native; a large or
// multi-component goal warrants the supervisor.
func TestChatShouldSupervise(t *testing.T) {
	cases := []struct {
		goal string
		want bool
	}{
		{"fix the typo in README", false},
		{"rename Foo to Bar in handler.go", false},
		{"build a URL-shortener service with tests and a Dockerfile from scratch", true},
		{"scaffold a new microservice", true},
		{strings.Repeat("word ", 50), true}, // long surface → supervise
	}
	for _, c := range cases {
		if got := chatShouldSupervise(c.goal); got != c.want {
			t.Errorf("chatShouldSupervise(%q) = %v, want %v", clip(c.goal), got, c.want)
		}
	}
}

func clip(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// TestParseAddVerb covers the /add control verb parse (path or URL argument).
// TestResolveReadRoot validates that an existing path resolves to an absolute,
// symlink-resolved root and a missing path errors (so /add never registers a root
// the read/search tools cannot reach).
func TestResolveReadRoot(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveReadRoot(dir)
	if err != nil {
		t.Fatalf("resolveReadRoot(%q): %v", dir, err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved root not absolute: %q", got)
	}
	if _, err := resolveReadRoot(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("a missing path must be rejected")
	}
}

// TestApplyAddVerbAddsRoot drives the full /add <path> control path against a real
// Session and asserts the resolved root is registered (and shows up in ReadRootsNow).
func TestApplyAddVerbAddsRoot(t *testing.T) {
	dir := t.TempDir()
	sess := session.New("c", "local", dir, newMemLog(t))
	con := termui.New(io.Discard)

	applyAddVerb(context.Background(), sess, con, dir)

	roots := sess.ReadRootsNow()
	if len(roots) != 1 {
		t.Fatalf("ReadRootsNow = %v, want one root", roots)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	if roots[0] != resolved {
		t.Errorf("registered root = %q, want %q", roots[0], resolved)
	}
}

// TestApplyContainerEgress proves the egress wiring: a container box is routed
// through the allowlist proxy (bridge network + HTTP_PROXY via the runtime host
// alias), docker additionally gets the --add-host so it resolves on Linux, and an
// empty allowlist leaves the box default-deny (no network).
func TestApplyContainerEgress(t *testing.T) {
	egress := policy.Egress{Allowed: []string{"example.com"}}

	// podman: host.containers.internal, no --add-host.
	pod := sandbox.NewContainer("podman", "img", "/work")
	applyContainerEgress(pod, egress, "0.0.0.0:54321", "podman")
	if pod.Network != "bridge" {
		t.Errorf("podman egress: Network = %q, want bridge", pod.Network)
	}
	if pod.Env["HTTP_PROXY"] != "http://host.containers.internal:54321" {
		t.Errorf("podman proxy url = %q", pod.Env["HTTP_PROXY"])
	}
	if len(pod.ExtraHosts) != 0 {
		t.Errorf("podman should need no --add-host, got %v", pod.ExtraHosts)
	}

	// docker: host.docker.internal + --add-host.
	dock := sandbox.NewContainer("docker", "img", "/work")
	applyContainerEgress(dock, egress, "0.0.0.0:54321", "docker")
	if dock.Env["HTTP_PROXY"] != "http://host.docker.internal:54321" {
		t.Errorf("docker proxy url = %q", dock.Env["HTTP_PROXY"])
	}
	if len(dock.ExtraHosts) != 1 || dock.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Errorf("docker --add-host = %v", dock.ExtraHosts)
	}

	// Empty allowlist ⇒ untouched (default-deny stays).
	deny := sandbox.NewContainer("podman", "img", "/work")
	applyContainerEgress(deny, policy.Egress{}, "", "podman")
	if deny.Network != "none" {
		t.Errorf("no allowlist must stay --network none, got %q", deny.Network)
	}
}

func TestWebEnabled(t *testing.T) {
	off := chatDeps{}
	if off.webEnabled() {
		t.Error("no egress configured: webEnabled must be false")
	}
	on := chatDeps{egress: policy.Egress{Allowed: []string{"x.com"}}, egressProxyAddr: "0.0.0.0:1"}
	if !on.webEnabled() {
		t.Error("egress + proxy addr: webEnabled must be true")
	}
	// Allowlist but no proxy (proxy failed to start) ⇒ still off (fail-closed).
	half := chatDeps{egress: policy.Egress{Allowed: []string{"x.com"}}}
	if half.webEnabled() {
		t.Error("allowlist without a running proxy must be treated as off")
	}
}

// TestModeGlyphDistinct asserts each mode maps to a DISTINCT prompt glyph, so the
// user can see at a glance which mode they're in.
func TestModeGlyphDistinct(t *testing.T) {
	st := termui.New(io.Discard).Style()
	seen := map[string]session.Mode{}
	for _, m := range []session.Mode{session.ModeAuto, session.ModeDiscuss, session.ModePlan, session.ModeExecute} {
		g, paint := modeGlyph(m, st)
		if g == "" || paint == nil {
			t.Fatalf("mode %v: empty glyph or nil paint", m)
		}
		if prev, dup := seen[g]; dup {
			t.Errorf("mode %v shares glyph %q with %v — modes must look distinct", m, g, prev)
		}
		seen[g] = m
	}
}

// TestIsUnknownSlash: a leading-'/' typo that is not a real verb and not a steer is
// flagged as unknown (so the REPL warns instead of sending it to the model).
func TestIsUnknownSlash(t *testing.T) {
	cases := map[string]bool{
		"/foo":           true,  // a typo
		"/discus":        true,  // misspelled mode verb
		"/steer fix it":  false, // a steer message, not a command
		"/steer":         false, // bare steer
		"!correct it":    false, // bang-steer
		"just a message": false, // ordinary text
		"  /bar  ":       true,  // trimmed
		"":               false,
	}
	for in, want := range cases {
		if got := isUnknownSlash(in); got != want {
			t.Errorf("isUnknownSlash(%q) = %v, want %v", in, got, want)
		}
	}
}

// (Mode-verb and /add parsing now live in session.ParseControl — see
// internal/session/control_test.go for the full table, shared by both front doors.)

// TestCapabilityForMode is the enforcement assertion: the read-only modes
// (Discuss/Plan) get a write-free registry AND the shell switched off, so there is
// NO structural path to mutate the tree; Execute/Auto get the full write set with
// the shell on. This is the guarantee "/plan writes no code" rests on — a property
// of the wiring, not of the model behaving.
func TestCapabilityForMode(t *testing.T) {
	writeTools := []string{"write", "edit", "git"}

	for _, m := range []session.Mode{session.ModeDiscuss, session.ModePlan} {
		reg, _, disableShell := capabilityForMode(m)
		if !disableShell {
			t.Errorf("%v: shell must be disabled (DisableShell=false)", m)
		}
		for _, name := range writeTools {
			if reg.Has(name) {
				t.Errorf("%v: read-only registry must NOT advertise %q", m, name)
			}
		}
		if !reg.Has("read") || !reg.Has("search") || !reg.Has("codeintel") {
			t.Errorf("%v: read-only registry must offer read/search/codeintel", m)
		}
	}

	for _, m := range []session.Mode{session.ModeExecute, session.ModeAuto} {
		reg, _, disableShell := capabilityForMode(m)
		if disableShell {
			t.Errorf("%v: shell must be enabled for a write-capable mode", m)
		}
		for _, name := range writeTools {
			if !reg.Has(name) {
				t.Errorf("%v: write-capable registry must advertise %q", m, name)
			}
		}
	}
}

// --- test doubles -----------------------------------------------------------

// newChatFlags builds a chatFlags with run-path defaults for a hermetic test (no
// container is ever launched — these only shape the Session/driver construction).
func newChatFlags(dir string) chatFlags {
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	f := func(v float64) *float64 { return &v }
	return chatFlags{
		common: commonFlags{
			dir:             s(dir),
			backendName:     s("native"),
			runtime:         s("podman"),
			image:           s("debian:stable-slim"),
			checkCmd:        s("true"),
			logPath:         s("nilcore.events.jsonl"),
			config:          s(""),
			maxSteps:        i(8),
			advisorMaxCalls: i(4),
			escalateAfter:   i(2),
		},
		budget: f(chatDefaultBudget),
	}
}

func newMemLog(t *testing.T) *eventlog.Log {
	t.Helper()
	log, err := eventlog.Open(t.TempDir() + "/events.jsonl")
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log
}

// chatFakeProvider is a scripted model.Provider for the wiring test: every Complete
// returns a fixed response with the configured usage so a metered wrap charges a
// deterministic amount. It touches no network.
type chatFakeProvider struct {
	id    string
	usage model.Usage
}

func (f *chatFakeProvider) Model() string { return f.id }

func (f *chatFakeProvider) Complete(_ context.Context, _ string, _ []model.Message, _ []model.Tool, _ int) (model.Response, error) {
	return model.Response{
		Content:    []model.Block{{Type: "text", Text: `{"route":"native","reason":"small"}`}},
		StopReason: "end_turn",
		Usage:      f.usage,
	}, nil
}

// staticRouter always routes to a fixed Route (no model call) so the REPL tests
// reach a driver deterministically.
type staticRouter struct{ r session.Route }

func (s staticRouter) Route(context.Context, string, session.WorkState) (session.Route, error) {
	return s.r, nil
}

// blockingDriver holds a drive in the Working phase until released, so a test can
// observe mid-work follow-ups (queue/steer) reaching the session inbox. It records
// nothing about its input — its only job is to stay running.
type blockingDriver struct {
	releaseC chan struct{}
	once     sync.Once
}

func newBlockingDriver() *blockingDriver { return &blockingDriver{releaseC: make(chan struct{})} }

func (d *blockingDriver) Drive(ctx context.Context, _ session.DriveInput) (session.DriveResult, error) {
	select {
	case <-d.releaseC:
	case <-ctx.Done():
	}
	return session.DriveResult{Verified: true}, nil
}

func (d *blockingDriver) release() { d.once.Do(func() { close(d.releaseC) }) }

// scriptReader feeds pre-set lines to a bufio.Scanner one at a time: each next()
// unblocks exactly one Read of a line, so the test controls when the REPL sees the
// next typed line. close() signals EOF.
type scriptReader struct {
	lines []string
	pump  chan struct{}
	mu    sync.Mutex
	buf   string
	idx   int
	done  bool
}

func newScriptReader(lines ...string) *scriptReader {
	return &scriptReader{lines: lines, pump: make(chan struct{}, len(lines))}
}

// next releases the next line to the reader.
func (s *scriptReader) next() { s.pump <- struct{}{} }

// close signals EOF after the queued lines drain.
func (s *scriptReader) close() {
	s.mu.Lock()
	s.done = true
	s.mu.Unlock()
	close(s.pump)
}

func (s *scriptReader) Read(p []byte) (int, error) {
	s.mu.Lock()
	if s.buf != "" {
		n := copy(p, s.buf)
		s.buf = s.buf[n:]
		s.mu.Unlock()
		return n, nil
	}
	s.mu.Unlock()

	_, ok := <-s.pump
	if !ok {
		return 0, io.EOF
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx >= len(s.lines) {
		return 0, io.EOF
	}
	s.buf = s.lines[s.idx] + "\n"
	s.idx++
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}

// --- sync helpers -----------------------------------------------------------

func waitPhase(t *testing.T, sess *session.Session, want session.Phase) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if sess.PhaseNow() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase never reached %v (now %v)", want, sess.PhaseNow())
}

func waitInboxLen(t *testing.T, sess *session.Session, n int) bool {
	t.Helper()
	for i := 0; i < 200; i++ {
		if q := sess.Inbox.Drain(); len(q) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func waitSteer(t *testing.T, sess *session.Session) bool {
	t.Helper()
	select {
	case <-sess.Inbox.Steer():
		return true
	case <-time.After(time.Second):
		return false
	}
}

// waitGoroutines lets transient goroutines settle, returning a stable count for a
// leak assertion (mirrors the native/super inbox suites' helper).
func waitGoroutines(want int) int {
	for i := 0; i < 50; i++ {
		if g := runtime.NumGoroutine(); g <= want {
			return g
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}
