package cdp

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

// TestComputeAcceptKey pins the RFC6455 example: the spec's sample client key
// "dGhlIHNhbXBsZSBub25jZQ==" must derive "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=".
func TestComputeAcceptKey(t *testing.T) {
	got := computeAcceptKey("dGhlIHNhbXBsZSBub25jZQ==")
	const want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Fatalf("computeAcceptKey = %q, want %q", got, want)
	}
}

// TestMaskPayloadRoundTrip verifies masking is its own inverse, since the same
// routine masks outbound and would unmask an (illegal-from-server) masked frame.
func TestMaskPayloadRoundTrip(t *testing.T) {
	key := [4]byte{0x01, 0x02, 0x03, 0x04}
	orig := []byte("the quick brown fox jumps over 13 lazy dogs")
	buf := make([]byte, len(orig))
	copy(buf, orig)

	maskPayload(buf, key)
	if bytes.Equal(buf, orig) {
		t.Fatal("masking did not change the payload")
	}
	maskPayload(buf, key) // unmask
	if !bytes.Equal(buf, orig) {
		t.Fatalf("round-trip mismatch: got %q, want %q", buf, orig)
	}
}

// TestEncodeDecodeFrame exercises the codec across the three length classes
// (7-bit, 16-bit, 64-bit) by encoding a masked client frame, then reading it
// back as if it were an (unmasked) server frame after clearing the mask bit.
func TestEncodeDecodeFrame(t *testing.T) {
	tests := []struct {
		name string
		size int
	}{
		{"tiny 7-bit", 5},
		{"boundary 125", 125},
		{"16-bit 126", 126},
		{"16-bit large", 4096},
		{"64-bit", 70000},
	}
	key := [4]byte{0xAA, 0xBB, 0xCC, 0xDD}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{'x'}, tt.size)
			framed := encodeFrameWithKey(opText, payload, key)

			// Sanity: FIN+text opcode and the MASK bit must be set.
			if framed[0] != (0x80 | opText) {
				t.Fatalf("byte0 = %#x, want FIN|text", framed[0])
			}
			if framed[1]&0x80 == 0 {
				t.Fatal("client frame must set the MASK bit")
			}

			// Convert to a server-shaped frame: same bytes but with the mask bit
			// cleared and the masking key + masked payload replaced by the raw
			// payload. Easiest is to re-derive an unmasked frame for the reader.
			server := makeServerFrame(opText, true, payload)
			f, err := readFrame(bufio.NewReader(bytes.NewReader(server)))
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if f.opcode != opText || !f.fin {
				t.Fatalf("decoded opcode/fin = %#x/%v", f.opcode, f.fin)
			}
			if !bytes.Equal(f.data, payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(f.data), len(payload))
			}
		})
	}
}

// TestReadFrameRejectsMaskedServerFrame ensures a masked server frame (a
// protocol violation) is rejected rather than silently accepted.
func TestReadFrameRejectsMaskedServerFrame(t *testing.T) {
	// A masked server frame: set the mask bit and append a key + masked payload.
	key := [4]byte{1, 2, 3, 4}
	masked := encodeFrameWithKey(opText, []byte("hi"), key) // this is a *client* frame (mask bit set)
	if _, err := readFrame(bufio.NewReader(bytes.NewReader(masked))); err == nil {
		t.Fatal("readFrame accepted a masked frame; must reject")
	}
}

// makeServerFrame builds an unmasked server-shaped frame (FIN optional) for the
// frame reader's tests. It mirrors the header rules without the mask bit/key.
func makeServerFrame(opcode byte, fin bool, payload []byte) []byte {
	n := len(payload)
	b0 := opcode & 0x0F
	if fin {
		b0 |= 0x80
	}
	var header []byte
	switch {
	case n <= 125:
		header = []byte{b0, byte(n)}
	case n <= 0xFFFF:
		header = []byte{b0, 126, byte(n >> 8), byte(n)}
	default:
		header = []byte{
			b0, 127,
			byte(n >> 56), byte(n >> 48), byte(n >> 40), byte(n >> 32),
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n),
		}
	}
	return append(header, payload...)
}

// TestReadHandshakeResponse parses a canned 101 response and checks the headers
// are lower-cased and the trailing frame bytes stay buffered.
func TestReadHandshakeResponse(t *testing.T) {
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"\r\n" +
		"LEFTOVER" // would be the first frame's bytes
	br := bufio.NewReader(strings.NewReader(resp))
	status, headers, err := readHandshakeResponse(br)
	if err != nil {
		t.Fatalf("readHandshakeResponse: %v", err)
	}
	if status != 101 {
		t.Fatalf("status = %d, want 101", status)
	}
	if headers["upgrade"] != "websocket" {
		t.Fatalf("upgrade header = %q", headers["upgrade"])
	}
	if headers["sec-websocket-accept"] != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept header = %q", headers["sec-websocket-accept"])
	}
	rest, _ := br.ReadString('\n')
	if rest != "LEFTOVER" {
		t.Fatalf("leftover bytes = %q, want LEFTOVER", rest)
	}
}

// TestReadMessageHandlesPingAndFragments drives readMessage over an in-memory
// pipe that sends a ping, then a fragmented text message, and confirms the ping
// is answered with a pong and the fragments reassemble.
func TestReadMessageHandlesPingAndFragments(t *testing.T) {
	clientWS, serverPipe := newPipePair(t)
	defer clientWS.close()

	go func() {
		// Server sends: ping("p"), then text fragment "Hel" (fin=false),
		// then continuation "lo!" (fin=true).
		writeServerFrame(t, serverPipe, makeServerFrame(opPing, true, []byte("p")))
		writeServerFrame(t, serverPipe, makeServerFrame(opText, false, []byte("Hel")))
		writeServerFrame(t, serverPipe, makeServerFrame(opContinuation, true, []byte("lo!")))
	}()

	msg, err := clientWS.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	if string(msg) != "Hello!" {
		t.Fatalf("reassembled = %q, want Hello!", msg)
	}

	// The client should have written a pong in response to the ping. Client frames
	// are masked, so read the raw opcode off the server end rather than via
	// readFrame (which rejects masked frames by design).
	if op, _ := peerFrame(t, bufio.NewReader(serverPipe)); op != opPong {
		t.Fatalf("expected pong opcode, got %#x", op)
	}
}

// TestReadMessageClose returns the close sentinel on a server close frame.
func TestReadMessageClose(t *testing.T) {
	clientWS, serverPipe := newPipePair(t)
	defer clientWS.close()
	go func() { writeServerFrame(t, serverPipe, makeServerFrame(opClose, true, []byte{0x03, 0xE8})) }()

	if _, err := clientWS.readMessage(); err == nil {
		t.Fatal("readMessage on close = nil error, want errClosed")
	}
}
