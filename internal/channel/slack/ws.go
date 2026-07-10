package slack

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// wsConn is a minimal RFC-6455 WebSocket client — just what Slack Socket Mode
// needs (read text frames, reply to pings, send masked text acks). Implemented
// on stdlib so the zero-dependency invariant (I6) holds; Socket Mode is the only
// place NilCore needs a WebSocket.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialWS opens a TLS WebSocket to a wss:// URL and completes the upgrade.
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":443"
	}
	d := &tls.Dialer{Config: &tls.Config{ServerName: u.Hostname()}}
	conn, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		u.RequestURI(), u.Hostname(), key)

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws handshake: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("ws handshake status %d", resp.StatusCode)
	}
	return &wsConn{conn: conn, r: r}, nil
}

// ReadText returns the next text-frame payload, transparently answering pings.
func (w *wsConn) ReadText() (string, error) {
	for {
		op, payload, err := w.readFrame()
		if err != nil {
			return "", err
		}
		switch op {
		case 0x1: // text
			return string(payload), nil
		case 0x9: // ping → pong
			if err := w.writeFrame(0xA, payload); err != nil {
				return "", err
			}
		case 0x8: // close
			return "", io.EOF
		}
	}
}

// WriteText sends a (masked) text frame.
func (w *wsConn) WriteText(s string) error { return w.writeFrame(0x1, []byte(s)) }

// Close closes the underlying connection.
func (w *wsConn) Close() error { return w.conn.Close() }

// maxWSFrameBytes bounds a single inbound WebSocket frame so a crafted length header
// can't OOM/panic the process on allocation (parity with internal/cdp's 64 MiB cap).
// Slack control/text frames are tiny; a real payload never approaches this.
const maxWSFrameBytes = 64 << 20

func (w *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	h := make([]byte, 2)
	if _, err = io.ReadFull(w.r, h); err != nil {
		return 0, nil, err
	}
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	length := int(h[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(w.r, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(w.r, ext); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(ext))
	}
	// Bound an untrusted frame length before allocating, so a crafted header can't
	// OOM/panic the process on `make` (length<0 catches a >maxInt Uint64 wrap).
	if length < 0 || length > maxWSFrameBytes {
		return 0, nil, fmt.Errorf("slack ws: frame length %d exceeds the %d-byte cap", length, maxWSFrameBytes)
	}
	var mask []byte
	if masked {
		mask = make([]byte, 4)
		if _, err = io.ReadFull(w.r, mask); err != nil {
			return 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(w.r, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	hdr := []byte{0x80 | opcode} // FIN + opcode
	l := len(payload)
	switch {
	case l < 126:
		hdr = append(hdr, 0x80|byte(l)) // mask bit + len
	case l < 65536:
		hdr = append(hdr, 0x80|126)
		ext := make([]byte, 2)
		binary.BigEndian.PutUint16(ext, uint16(l))
		hdr = append(hdr, ext...)
	default:
		hdr = append(hdr, 0x80|127)
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(l))
		hdr = append(hdr, ext...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	hdr = append(hdr, mask...)
	masked := make([]byte, l)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.conn.Write(append(hdr, masked...))
	return err
}
