package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// newPipePair wires a client wsConn to an in-memory peer over a loopback TCP
// socket pair so the CDP and WebSocket layers can be exercised with no Chrome.
// We use a real (kernel-buffered) localhost socket rather than net.Pipe because
// net.Pipe is fully synchronous — a client write (e.g. a pong answering a server
// ping) would block until the peer reads it, deadlocking flows where both sides
// write before either reads. The listener binds 127.0.0.1:0 (ephemeral) and is
// torn down in cleanup; nothing leaves the host, so the test stays hermetic.
// The returned net.Conn is the *server* end: a test reads client frames off it
// and writes server frames (built with makeServerFrame) back.
func newPipePair(t *testing.T) (*wsConn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen loopback: %v", err)
	}
	defer ln.Close()

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- accepted{conn: c, err: err}
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial loopback: %v", err)
	}
	a := <-acceptCh
	if a.err != nil {
		t.Fatalf("accept loopback: %v", a.err)
	}
	t.Cleanup(func() { _ = clientConn.Close(); _ = a.conn.Close() })
	return &wsConn{conn: clientConn, br: bufio.NewReader(clientConn)}, a.conn
}

// writeServerResult marshals a CDP response with the given id+result and writes
// it as an unmasked server text frame.
func writeServerResult(t *testing.T, server net.Conn, id int64, result any) {
	t.Helper()
	writeServerJSON(t, server, struct {
		ID     int64 `json:"id"`
		Result any   `json:"result"`
	}{ID: id, Result: result})
}

// peerReq is the decoded shape of a client→server CDP request, sufficient for
// every assertion the tests make about Send's envelope.
type peerReq struct {
	ID     int64          `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// peerFrame reads one masked client frame off the peer end, returning its opcode
// and unmasked payload. Client frames are masked (RFC6455 §5.3), so we parse the
// header by hand and unmask. It fails the test on any read error.
func peerFrame(t *testing.T, br *bufio.Reader) (opcode byte, payload []byte) {
	t.Helper()
	var head [2]byte
	mustReadFull(t, br, head[:])
	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	n := int(head[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		mustReadFull(t, br, ext[:])
		n = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		mustReadFull(t, br, ext[:])
		n = 0
		for _, b := range ext {
			n = n<<8 | int(b)
		}
	}
	var key [4]byte
	if masked {
		mustReadFull(t, br, key[:])
	}
	data := make([]byte, n)
	mustReadFull(t, br, data)
	if masked {
		maskPayload(data, key)
	}
	return opcode, data
}

// decodeClientRequest reads one client text frame and decodes its JSON-RPC
// request. It centralizes the read+unmarshal so call sites stay terse and every
// error is checked (lint-clean).
func decodeClientRequest(t *testing.T, br *bufio.Reader) peerReq {
	t.Helper()
	op, payload := peerFrame(t, br)
	if op != opText {
		t.Fatalf("client frame opcode = %#x, want text", op)
	}
	var req peerReq
	if err := json.Unmarshal(payload, &req); err != nil {
		t.Fatalf("decoding client request: %v", err)
	}
	return req
}

// writeServerJSON marshals v and writes it as an unmasked server text frame.
func writeServerJSON(t *testing.T, server net.Conn, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling server frame: %v", err)
	}
	if _, err := server.Write(makeServerFrame(opText, true, body)); err != nil {
		t.Fatalf("writing server frame: %v", err)
	}
}

// writeServerFrame writes a pre-built server frame to the peer, checking the
// write error. Used by the WebSocket-codec tests that send raw control/data
// frames rather than JSON.
func writeServerFrame(t *testing.T, server net.Conn, frame []byte) {
	t.Helper()
	if _, err := server.Write(frame); err != nil {
		t.Fatalf("writing server frame: %v", err)
	}
}

func mustReadFull(t *testing.T, r *bufio.Reader, p []byte) {
	t.Helper()
	total := 0
	for total < len(p) {
		n, err := r.Read(p[total:])
		total += n
		if err != nil {
			t.Fatalf("peer read: %v", err)
		}
	}
}

// TestSendRequestShape asserts that Send marshals the exact CDP envelope —
// method, params, and a monotonically increasing id — and threads the result
// back. A goroutine plays the peer: it reads the masked request, checks the
// shape, and replies with a canned result.
func TestSendRequestShape(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	type capture struct {
		method string
		params map[string]any
		id     int64
	}
	got := make(chan capture, 1)
	go func() {
		req := decodeClientRequest(t, bufio.NewReader(server))
		got <- capture{method: req.Method, params: req.Params, id: req.ID}
		// Reply with the id the client used.
		writeServerResult(t, server, req.ID, map[string]any{"frameId": "F1"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := c.Send(ctx, "Page.navigate", map[string]any{"url": "http://x/"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	cap := <-got
	if cap.method != "Page.navigate" {
		t.Fatalf("method = %q, want Page.navigate", cap.method)
	}
	if cap.params["url"] != "http://x/" {
		t.Fatalf("params.url = %v, want http://x/", cap.params["url"])
	}
	if cap.id != 1 {
		t.Fatalf("first request id = %v, want 1", cap.id)
	}
	var rmap map[string]string
	if err := json.Unmarshal(res, &rmap); err != nil || rmap["frameId"] != "F1" {
		t.Fatalf("result = %s, err=%v", res, err)
	}
}

// TestSendIDsIncrement confirms ids advance across calls (the JSON-RPC matching
// key). We drive two sequential Sends through the peer.
func TestSendIDsIncrement(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	ids := make(chan int64, 2)
	go func() {
		br := bufio.NewReader(server)
		for i := 0; i < 2; i++ {
			req := decodeClientRequest(t, br)
			ids <- req.ID
			writeServerResult(t, server, req.ID, map[string]any{})
		}
	}()

	ctx := context.Background()
	if _, err := c.Send(ctx, "Page.enable", nil); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := c.Send(ctx, "Runtime.enable", nil); err != nil {
		t.Fatalf("second send: %v", err)
	}
	first, second := <-ids, <-ids
	if first != 1 || second != 2 {
		t.Fatalf("ids = %v,%v want 1,2", first, second)
	}
}

// TestSendSkipsEvents confirms Send ignores interleaved CDP events (a message
// with a method but no id) and returns only the matching reply.
func TestSendSkipsEvents(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	go func() {
		req := decodeClientRequest(t, bufio.NewReader(server))
		// Emit an event first, then the real reply.
		writeServerJSON(t, server, map[string]any{"method": "Page.frameNavigated", "params": map[string]any{}})
		writeServerResult(t, server, req.ID, map[string]any{"ok": true})
	}()

	res, err := c.Send(context.Background(), "Page.enable", nil)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	var m map[string]bool
	if err := json.Unmarshal(res, &m); err != nil || !m["ok"] {
		t.Fatalf("result = %s err=%v, want ok:true", res, err)
	}
}

// TestSendSurfacesCDPError maps a CDP error object to a Go error.
func TestSendSurfacesCDPError(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	go func() {
		req := decodeClientRequest(t, bufio.NewReader(server))
		writeServerJSON(t, server, map[string]any{
			"id":    req.ID,
			"error": map[string]any{"code": -32000, "message": "boom"},
		})
	}()

	if _, err := c.Send(context.Background(), "Page.navigate", nil); err == nil {
		t.Fatal("Send with CDP error = nil, want error")
	}
}

// TestEvalDecodesValue exercises the typed Eval path: the peer returns a
// Runtime.evaluate result whose result.value is a string, and EvalString yields
// it. This also proves the Runtime.evaluate params shape is what we send.
func TestEvalDecodesValue(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	go func() {
		req := decodeClientRequest(t, bufio.NewReader(server))
		if req.Method != "Runtime.evaluate" {
			t.Errorf("method = %q, want Runtime.evaluate", req.Method)
		}
		if req.Params["returnByValue"] != true {
			t.Errorf("returnByValue not set: %v", req.Params)
		}
		writeServerResult(t, server, req.ID, map[string]any{
			"result": map[string]any{"type": "string", "value": "My Title"},
		})
	}()

	got, err := c.EvalString(context.Background(), "document.title")
	if err != nil {
		t.Fatalf("EvalString: %v", err)
	}
	if got != "My Title" {
		t.Fatalf("EvalString = %q, want My Title", got)
	}
}

// TestElementCenterDecodes confirms ElementCenter parses an {x,y} value and that
// a null (no match) is an error.
func TestElementCenterDecodes(t *testing.T) {
	t.Run("match", func(t *testing.T) {
		clientWS, server := newPipePair(t)
		c := &Conn{ws: clientWS}
		go func() {
			req := decodeClientRequest(t, bufio.NewReader(server))
			writeServerResult(t, server, req.ID, map[string]any{
				"result": map[string]any{"value": map[string]any{"x": 10.5, "y": 20.0}},
			})
		}()
		pt, err := c.ElementCenter(context.Background(), "#login")
		if err != nil {
			t.Fatalf("ElementCenter: %v", err)
		}
		if pt.X != 10.5 || pt.Y != 20.0 {
			t.Fatalf("center = %+v, want {10.5 20}", pt)
		}
	})
	t.Run("no match", func(t *testing.T) {
		clientWS, server := newPipePair(t)
		// A sub-tick budget makes the auto-wait poll exactly once (any real round-trip
		// exceeds 1ns), so the single-null server below suffices and the genuine
		// no-match path still returns the precise domain error fast.
		c := &Conn{ws: clientWS, actionWait: time.Nanosecond}
		go func() {
			req := decodeClientRequest(t, bufio.NewReader(server))
			writeServerResult(t, server, req.ID, map[string]any{
				"result": map[string]any{"value": nil},
			})
		}()
		_, err := c.ElementCenter(context.Background(), "#missing")
		if err == nil {
			t.Fatal("ElementCenter on no-match = nil, want error")
		}
		if !strings.Contains(err.Error(), "matched no visible element") {
			t.Fatalf("error = %v, want a 'matched no visible element' domain error", err)
		}
	})
}

// TestElementCenterAutoWaitsForElement proves the auto-wait: when the element is not
// present on the first DOM queries (the page is still settling) but appears on a later
// poll, ElementCenter waits for it and returns its center rather than failing on the
// first miss — the settle race the live-flow CI exercised.
func TestElementCenterAutoWaitsForElement(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS} // default budget; the element appears well within it
	go func() {
		br := bufio.NewReader(server)
		// First two polls see nothing (null); the third sees the laid-out element.
		for i := 0; i < 3; i++ {
			req := decodeClientRequest(t, br)
			if i < 2 {
				writeServerResult(t, server, req.ID, map[string]any{
					"result": map[string]any{"value": nil},
				})
				continue
			}
			writeServerResult(t, server, req.ID, map[string]any{
				"result": map[string]any{"value": map[string]any{"x": 10.5, "y": 20.0}},
			})
		}
	}()
	pt, err := c.ElementCenter(context.Background(), "#late")
	if err != nil {
		t.Fatalf("ElementCenter with a late element: %v", err)
	}
	if pt.X != 10.5 || pt.Y != 20.0 {
		t.Fatalf("center = %+v, want {10.5 20} after the auto-wait", pt)
	}
}

// TestClickDispatchesPressAndRelease confirms a Click sends exactly two
// Input.dispatchMouseEvent commands (mousePressed then mouseReleased).
func TestClickDispatchesPressAndRelease(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	types := make(chan string, 2)
	go func() {
		br := bufio.NewReader(server)
		for i := 0; i < 2; i++ {
			req := decodeClientRequest(t, br)
			if req.Method != "Input.dispatchMouseEvent" {
				t.Errorf("method = %q, want Input.dispatchMouseEvent", req.Method)
			}
			types <- req.Params["type"].(string)
			writeServerResult(t, server, req.ID, map[string]any{})
		}
	}()

	if err := c.Click(context.Background(), 5, 6); err != nil {
		t.Fatalf("Click: %v", err)
	}
	if a, b := <-types, <-types; a != "mousePressed" || b != "mouseReleased" {
		t.Fatalf("mouse event types = %q,%q want mousePressed,mouseReleased", a, b)
	}
}

// TestClickButtonsBitmask confirms `buttons` reflects which buttons are held during
// each phase: 1 on press, 0 on release (CDP semantics).
func TestClickButtonsBitmask(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	reqs := make(chan peerReq, 2)
	go func() {
		br := bufio.NewReader(server)
		for i := 0; i < 2; i++ {
			req := decodeClientRequest(t, br)
			reqs <- req
			writeServerResult(t, server, req.ID, map[string]any{})
		}
	}()

	if err := c.Click(context.Background(), 1, 2); err != nil {
		t.Fatalf("Click: %v", err)
	}
	down, up := <-reqs, <-reqs
	if b, _ := down.Params["buttons"].(float64); b != 1 {
		t.Errorf("mousePressed buttons = %v, want 1", down.Params["buttons"])
	}
	if b, _ := up.Params["buttons"].(float64); b != 0 {
		t.Errorf("mouseReleased buttons = %v, want 0", up.Params["buttons"])
	}
}

// TestTypeKeyEnterCarriesVirtualKeyCode confirms a discrete Enter press carries the
// metadata Chrome needs to fire the default action (form submit) — not a bare,
// inert {type,key} event.
func TestTypeKeyEnterCarriesVirtualKeyCode(t *testing.T) {
	clientWS, server := newPipePair(t)
	c := &Conn{ws: clientWS}

	reqs := make(chan peerReq, 2)
	go func() {
		br := bufio.NewReader(server)
		for i := 0; i < 2; i++ {
			req := decodeClientRequest(t, br)
			reqs <- req
			writeServerResult(t, server, req.ID, map[string]any{})
		}
	}()

	if err := c.TypeKey(context.Background(), "Enter"); err != nil {
		t.Fatalf("TypeKey: %v", err)
	}
	down, up := <-reqs, <-reqs
	if down.Method != "Input.dispatchKeyEvent" || down.Params["type"] != "keyDown" {
		t.Fatalf("first event = %q/%v, want Input.dispatchKeyEvent/keyDown", down.Method, down.Params["type"])
	}
	if vk, _ := down.Params["windowsVirtualKeyCode"].(float64); vk != 13 {
		t.Errorf("Enter keyDown windowsVirtualKeyCode = %v, want 13", down.Params["windowsVirtualKeyCode"])
	}
	if down.Params["code"] != "Enter" || down.Params["text"] != "\r" {
		t.Errorf("Enter keyDown code/text = %v/%v, want Enter/CR", down.Params["code"], down.Params["text"])
	}
	if up.Params["type"] != "keyUp" {
		t.Errorf("second event type = %v, want keyUp", up.Params["type"])
	}
}
