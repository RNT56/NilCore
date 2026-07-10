package slack

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// frameHeader127 builds a two-byte WebSocket header that declares an 8-byte extended
// length (indicator 127), unmasked, followed by the big-endian length. No payload
// follows — a correct reader must reject the frame on the length alone, before it ever
// tries to allocate a payload buffer.
func frameHeader127(length uint64) []byte {
	h := make([]byte, 10)
	h[0] = 0x81 // FIN + text opcode
	h[1] = 0x7f // unmasked, length indicator 127 -> read 8 more length bytes
	binary.BigEndian.PutUint64(h[2:], length)
	return h
}

// TestReadFrameRejectsOversizedLength proves the frame-length cap: a crafted 8-byte
// extended length that exceeds maxWSFrameBytes, or that wraps negative when narrowed to
// int, is rejected with an error BEFORE `payload = make([]byte, length)`. Without the
// guard the oversized case would allocate (then fail on the missing payload with an EOF,
// not the cap error) and the wrap-negative case would `make([]byte, <negative>)` and
// PANIC. Asserting the returned error names the cap ("exceeds") — not an EOF, not a
// panic — is what makes this discriminating for the guard specifically.
func TestReadFrameRejectsOversizedLength(t *testing.T) {
	for _, tc := range []struct {
		name   string
		length uint64
	}{
		{"over-cap", uint64(maxWSFrameBytes) + 1},
		// High bit set: Uint64 -> int64 narrows to a negative length (MinInt64),
		// which an unguarded make() would reject with a panic.
		{"wrap-negative", 0x8000000000000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &wsConn{r: bufio.NewReader(bytes.NewReader(frameHeader127(tc.length)))}
			// If the guard were missing, the wrap-negative case would panic here rather
			// than return; reaching the assertions at all already proves it did not.
			op, payload, err := w.readFrame()
			if err == nil {
				t.Fatalf("expected an error for a %d-byte frame length, got op=%x payload len=%d", tc.length, op, len(payload))
			}
			if payload != nil {
				t.Errorf("no payload should be allocated for a rejected frame, got %d bytes", len(payload))
			}
			if !strings.Contains(err.Error(), "exceeds") {
				t.Errorf("error should name the frame-length cap (got %q); an EOF here would mean the huge make() ran", err)
			}
		})
	}
}
