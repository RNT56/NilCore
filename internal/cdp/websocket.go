// Package cdp is a minimal, pure-Go client for Chrome's DevTools Protocol (CDP)
// over a localhost WebSocket. It exists so the in-sandbox browser driver can
// drive an interactive *flow* (navigate → click → type → wait → observe), not
// just capture a one-shot page. It is deliberately tiny and stdlib-only (I6):
// net for the TCP dial, crypto/sha1+crypto/rand+encoding/base64 for the RFC6455
// handshake and frame masking, encoding/json for the JSON-RPC envelope, and
// bufio for buffered reads. There is NO TLS — CDP's debugging endpoint is
// plaintext ws:// bound to loopback, and we refuse anything else.
//
// Everything Chrome sends back (page text, titles, screenshots, console) is
// UNTRUSTED data (I7): this package only transports and decodes it, never lets
// it steer control flow.
package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

// wsGUID is the RFC6455 magic string concatenated with the client key to derive
// the Sec-WebSocket-Accept value. It is fixed by the spec.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket opcodes we handle. CDP traffic is all text frames; control frames
// (ping/pong/close) are handled to keep the connection healthy and to shut down
// cleanly. Binary frames are not used by CDP, so we reject them.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxFramePayload bounds a single inbound frame so a misbehaving or hostile peer
// cannot make us allocate without limit. Screenshots arrive as one large text
// frame (base64 PNG), so the ceiling is generous but finite.
const maxFramePayload = 64 << 20 // 64 MiB

// wsConn is a minimal RFC6455 client connection over an already-dialed TCP
// socket. It is not safe for concurrent writers; the CDP layer above serializes
// requests, which is all we need.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// computeAcceptKey derives the Sec-WebSocket-Accept response value from the
// client's Sec-WebSocket-Key, per RFC6455 §4.2.2: base64(sha1(key + GUID)). It
// is exported-for-test via the package-internal test; kept pure for hermetic
// unit testing without a socket.
func computeAcceptKey(clientKey string) string {
	h := sha1.New() //nolint:gosec // RFC6455 mandates SHA-1 for the handshake; not a security primitive here.
	h.Write([]byte(clientKey + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// newClientKey returns a fresh, base64-encoded 16-byte nonce for the handshake
// (RFC6455 §4.1). crypto/rand makes each handshake unique so a cached/forged
// Accept cannot be replayed.
func newClientKey() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating websocket key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(nonce[:]), nil
}

// dialWebSocket performs the HTTP Upgrade handshake against a ws:// URL on
// localhost and returns a ready wsConn. It refuses any scheme other than ws and
// any host other than loopback — this transport is for Chrome's local debugging
// endpoint only, never a remote server (defense in depth around I3/I7). The
// context bounds the dial.
func dialWebSocket(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing websocket url: %w", err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("websocket scheme must be ws (got %q): TLS/remote endpoints are refused", u.Scheme)
	}
	host := u.Hostname()
	if !isLoopback(host) {
		return nil, fmt.Errorf("websocket host must be loopback (got %q)", host)
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("dialing websocket: %w", err)
	}

	// Apply the context deadline to the handshake I/O so a wedged peer cannot
	// hang us before the connection is usable.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	ws := &wsConn{conn: conn, br: bufio.NewReader(conn)}
	if err := ws.handshake(u); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Clear the handshake deadline; per-call deadlines are set by callers.
	_ = conn.SetDeadline(time.Time{})
	return ws, nil
}

// isLoopback reports whether host names the local machine. We accept the literal
// loopback IPs and "localhost" so the resolver is never consulted for a remote
// name (the endpoint is always Chrome on this host).
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handshake writes the client Upgrade request and validates the server's
// switching-protocols response, including the Sec-WebSocket-Accept value.
func (ws *wsConn) handshake(u *url.URL) error {
	key, err := newClientKey()
	if err != nil {
		return err
	}
	reqPath := u.RequestURI()
	if reqPath == "" {
		reqPath = "/"
	}

	// A bare, spec-minimal Upgrade request. We send Host + the three required
	// WebSocket headers and nothing else.
	req := strings.Join([]string{
		"GET " + reqPath + " HTTP/1.1",
		"Host: " + u.Host,
		"Upgrade: websocket",
		"Connection: Upgrade",
		"Sec-WebSocket-Key: " + key,
		"Sec-WebSocket-Version: 13",
		"", "",
	}, "\r\n")
	if _, err := ws.conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("writing handshake: %w", err)
	}

	status, headers, err := readHandshakeResponse(ws.br)
	if err != nil {
		return err
	}
	if status != 101 {
		return fmt.Errorf("websocket upgrade rejected: HTTP status %d", status)
	}
	if !strings.EqualFold(headers["upgrade"], "websocket") {
		return fmt.Errorf("missing/invalid Upgrade header in handshake response")
	}
	want := computeAcceptKey(key)
	if headers["sec-websocket-accept"] != want {
		return errors.New("websocket Sec-WebSocket-Accept mismatch (handshake forged or corrupt)")
	}
	return nil
}

// readHandshakeResponse parses the status line and headers of the server's
// HTTP/1.1 handshake response. Header names are lower-cased for case-insensitive
// lookup. It stops at the blank line that ends the header block; any frame bytes
// after that stay buffered in br for the frame reader.
func readHandshakeResponse(br *bufio.Reader) (status int, headers map[string]string, err error) {
	statusLine, err := readLine(br)
	if err != nil {
		return 0, nil, fmt.Errorf("reading handshake status line: %w", err)
	}
	// Status line: "HTTP/1.1 101 Switching Protocols".
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, nil, fmt.Errorf("malformed handshake status line %q", statusLine)
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &status); err != nil {
		return 0, nil, fmt.Errorf("parsing handshake status code %q: %w", parts[1], err)
	}

	headers = make(map[string]string)
	for {
		line, err := readLine(br)
		if err != nil {
			return 0, nil, fmt.Errorf("reading handshake headers: %w", err)
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return status, headers, nil
}

// readLine reads a single CRLF-terminated line and returns it without the
// trailing CRLF. A bare LF is tolerated to be liberal in what we accept.
func readLine(br *bufio.Reader) (string, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// ───────────────────────────── frame codec ─────────────────────────────

// maskPayload XORs payload in place with the 4-byte masking key. RFC6455
// requires every client→server frame to be masked; the same routine unmasks
// (the operation is its own inverse). Exposed within the package so the codec
// tests can exercise the round-trip directly.
func maskPayload(payload []byte, key [4]byte) {
	for i := range payload {
		payload[i] ^= key[i&3]
	}
}

// encodeFrame builds a single masked client frame for the given opcode and
// payload. We always send unfragmented frames (FIN=1) with a fresh random mask,
// as RFC6455 §5.3 requires of clients. The mask key comes from crypto/rand.
func encodeFrame(opcode byte, payload []byte) ([]byte, error) {
	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("generating frame mask: %w", err)
	}
	return encodeFrameWithKey(opcode, payload, key), nil
}

// encodeFrameWithKey is the deterministic core of encodeFrame, split out so the
// codec can be unit-tested with a fixed key (the random path just calls this).
func encodeFrameWithKey(opcode byte, payload []byte, key [4]byte) []byte {
	n := len(payload)
	var header []byte
	// FIN bit set (0x80) | opcode. We never fragment outbound frames.
	b0 := byte(0x80) | (opcode & 0x0F)

	switch {
	case n <= 125:
		header = []byte{b0, byte(0x80 | n)} // 0x80 = MASK bit set.
	case n <= 0xFFFF:
		header = []byte{b0, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		header = []byte{
			b0, 0x80 | 127,
			byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
		}
	}
	header = append(header, key[:]...)

	masked := make([]byte, n)
	copy(masked, payload)
	maskPayload(masked, key)
	return append(header, masked...)
}

// frame is a decoded inbound WebSocket frame. Server→client frames are never
// masked, so we only carry the opcode, FIN flag, and payload.
type frame struct {
	opcode byte
	fin    bool
	data   []byte
}

// readFrame reads one frame off the wire. It enforces that server frames are
// unmasked (RFC6455 §5.1) and bounds the payload length. It does not assemble
// fragments — readMessage does that, treating this as the per-frame primitive.
func readFrame(br *bufio.Reader) (frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return frame{}, fmt.Errorf("reading frame header: %w", err)
	}
	fin := head[0]&0x80 != 0
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	if masked {
		// A compliant server never masks; a masked frame means we are talking to
		// something we don't trust to be Chrome's endpoint.
		return frame{}, errors.New("server sent a masked frame (protocol violation)")
	}

	length := int(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return frame{}, fmt.Errorf("reading 16-bit length: %w", err)
		}
		length = int(ext[0])<<8 | int(ext[1])
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return frame{}, fmt.Errorf("reading 64-bit length: %w", err)
		}
		var l uint64
		for _, b := range ext {
			l = l<<8 | uint64(b)
		}
		if l > maxFramePayload {
			return frame{}, fmt.Errorf("frame payload %d exceeds limit %d", l, maxFramePayload)
		}
		length = int(l)
	}
	if length > maxFramePayload {
		return frame{}, fmt.Errorf("frame payload %d exceeds limit %d", length, maxFramePayload)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(br, data); err != nil {
		return frame{}, fmt.Errorf("reading frame payload: %w", err)
	}
	return frame{opcode: opcode, fin: fin, data: data}, nil
}

// ───────────────────────────── message I/O ─────────────────────────────

// writeText sends a text message as a single masked frame.
func (ws *wsConn) writeText(payload []byte) error {
	f, err := encodeFrame(opText, payload)
	if err != nil {
		return err
	}
	if _, err := ws.conn.Write(f); err != nil {
		return fmt.Errorf("writing text frame: %w", err)
	}
	return nil
}

// writeControl sends a control frame (ping/pong/close) with the given opcode.
func (ws *wsConn) writeControl(opcode byte, payload []byte) error {
	f, err := encodeFrame(opcode, payload)
	if err != nil {
		return err
	}
	if _, err := ws.conn.Write(f); err != nil {
		return fmt.Errorf("writing control frame: %w", err)
	}
	return nil
}

// readMessage returns the next complete *application* (text) message, handling
// control frames transparently: it answers pings with pongs, ignores pongs, and
// returns a sentinel error on close. Continuation frames are reassembled. Binary
// frames are rejected because CDP never uses them.
func (ws *wsConn) readMessage() ([]byte, error) {
	var buf []byte
	var assembling bool
	for {
		f, err := readFrame(ws.br)
		if err != nil {
			return nil, err
		}
		switch f.opcode {
		case opPing:
			// Echo the application data back as a pong (RFC6455 §5.5.2).
			if err := ws.writeControl(opPong, f.data); err != nil {
				return nil, err
			}
		case opPong:
			// Unsolicited or heartbeat pong; nothing to do.
		case opClose:
			return nil, errClosed
		case opText:
			if assembling {
				return nil, errors.New("unexpected text frame mid-fragment")
			}
			buf = append(buf, f.data...)
			if f.fin {
				return buf, nil
			}
			assembling = true
		case opContinuation:
			if !assembling {
				return nil, errors.New("continuation frame without a start frame")
			}
			buf = append(buf, f.data...)
			if f.fin {
				return buf, nil
			}
		case opBinary:
			return nil, errors.New("unexpected binary frame (CDP is text-only)")
		default:
			return nil, fmt.Errorf("unknown opcode 0x%X", f.opcode)
		}
	}
}

// errClosed is returned by readMessage when the peer sent a close frame. The CDP
// layer treats it as a clean end-of-stream.
var errClosed = errors.New("websocket closed by peer")

// close sends a close frame (best-effort) and tears down the TCP socket.
func (ws *wsConn) close() error {
	// 1000 = normal closure. Ignore the write error; we close the socket anyway.
	_ = ws.writeControl(opClose, []byte{0x03, 0xE8})
	return ws.conn.Close()
}

// setDeadline applies a read/write deadline to the underlying socket so a CDP
// call can be bounded by its context.
func (ws *wsConn) setDeadline(t time.Time) error {
	return ws.conn.SetDeadline(t)
}
