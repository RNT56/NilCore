package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Conn is a typed CDP client over a single WebSocket to a Chrome page target.
// It speaks the JSON-RPC dialect CDP uses: each request carries a monotonically
// increasing id, and the matching response echoes that id. Because the driver
// issues commands sequentially (navigate, then click, then screenshot), Conn
// serializes Send under a mutex and reads the next message inline — there is no
// background reader, which keeps the surface tiny and the failure modes obvious.
//
// All results coming back from Chrome are UNTRUSTED data (I7); Conn returns them
// as raw json.RawMessage for the caller to decode into a known shape.
type Conn struct {
	mu sync.Mutex
	ws *wsConn
	id int64
}

// rpcRequest is the CDP command envelope. params is omitted when nil so commands
// that take no arguments marshal cleanly.
type rpcRequest struct {
	ID     int64       `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params,omitempty"`
}

// rpcResponse is the CDP reply envelope. Either Result or Error is set. Events
// (messages with a "method" but no "id") are skipped by the reader.
type rpcResponse struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Method string          `json:"method"`
}

// rpcError is the CDP error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// Dial connects to a CDP page target's WebSocket debugger URL (ws:// on
// localhost) and returns a ready Conn. The context bounds the dial+handshake.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, err := dialWebSocket(ctx, wsURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to devtools endpoint: %w", err)
	}
	return &Conn{ws: ws}, nil
}

// Close shuts down the underlying socket.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.close()
}

// Send issues a CDP command and returns its result. params may be nil. It blocks
// until the response with the matching id arrives, skipping any interleaved CDP
// events. The context bounds the whole round-trip via a socket deadline.
func (c *Conn) Send(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	reqID := c.id

	// Bound the round-trip. A default applies when the caller passes no deadline
	// so a wedged browser cannot hang the driver indefinitely.
	deadline := time.Now().Add(defaultCallTimeout)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	if err := c.ws.setDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting call deadline: %w", err)
	}
	defer func() { _ = c.ws.setDeadline(time.Time{}) }()

	payload, err := json.Marshal(rpcRequest{ID: reqID, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("marshaling %s request: %w", method, err)
	}
	if err := c.ws.writeText(payload); err != nil {
		return nil, fmt.Errorf("sending %s: %w", method, err)
	}

	// Read until we see the response carrying our id; CDP may interleave async
	// events (which have a method but no id) ahead of the reply.
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg, err := c.ws.readMessage()
		if err != nil {
			if errors.Is(err, errClosed) {
				return nil, fmt.Errorf("devtools closed while awaiting %s: %w", method, err)
			}
			return nil, fmt.Errorf("reading %s response: %w", method, err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			return nil, fmt.Errorf("decoding %s response: %w", method, err)
		}
		if resp.ID == 0 && resp.Method != "" {
			// An event (e.g. Page.frameNavigated); not our reply — keep reading.
			continue
		}
		if resp.ID != reqID {
			// A reply to some other in-flight id should not happen given the mutex,
			// but skip defensively rather than mis-attributing it.
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s failed: %w", method, resp.Error)
		}
		return resp.Result, nil
	}
}

// defaultCallTimeout bounds a single CDP round-trip when the caller's context
// carries no deadline.
const defaultCallTimeout = 30 * time.Second
