package model

import "context"

// Chunk is one streamed delta surfaced to a caller as the model's reply is still
// being generated. It carries only the incremental output text of a single
// streaming event so a front end can paint tokens live.
//
// Text is the token text of this delta. It is empty for streaming events that
// carry no output text (message/content-block lifecycle frames, tool-call
// argument deltas, usage-only frames). A caller that is purely surfacing prose
// can ignore empty-Text chunks; the authoritative, fully-assembled reply is the
// returned Response, never the concatenation of chunks.
type Chunk struct {
	// Text is the incremental output text of this streamed event (empty for
	// non-text events).
	Text string
}

// Streamer is the OPTIONAL streaming counterpart to Provider.Complete. A
// Provider MAY also implement Streamer; the loop type-asserts for it and falls
// back to Complete when a provider does not. Implementing Streamer never changes
// Provider — Stream is purely ADDITIVE (invariant I1: the frozen backend
// contract and Provider.Complete are unchanged).
//
// Contract — a Streamer MUST honor all of the following:
//
//   - Same result as Complete. Stream assembles and returns the SAME full
//     Response that Complete(ctx, system, msgs, tools, maxTokens) would return
//     for the same inputs — identical Content blocks, StopReason, and Usage. The
//     stream is a delivery detail, not a different reply: a caller may use Stream
//     or Complete interchangeably and must get the same final value.
//
//   - Forward text as it arrives. As each output-text delta is decoded off the
//     wire, the Streamer calls onChunk with a Chunk whose Text is that delta,
//     BEFORE the full Response is complete. Non-text events (lifecycle frames,
//     tool-argument deltas) need not be forwarded; when they are, Chunk.Text is
//     empty. The concatenation of all forwarded Chunk.Text equals the output
//     text in the returned Response.
//
//   - onChunk MUST NOT block. The Streamer calls onChunk synchronously on its
//     read loop, so a slow callback stalls decoding. A caller that does heavy or
//     blocking work MUST hand it off (buffered channel / separate goroutine) and
//     return from onChunk promptly. onChunk is always invoked from the single
//     goroutine that calls Stream — never concurrently — so the callback needs no
//     internal locking. onChunk may be nil; a nil callback is treated as a no-op
//     (Stream still returns the same assembled Response).
//
//   - Interrupt-but-preserve on ctx cancellation. If ctx is cancelled (or its
//     deadline elapses) mid-stream, Stream STOPS reading and returns the PARTIAL
//     Response assembled from the deltas received so far TOGETHER WITH ctx.Err()
//     (the non-nil context error). This is the property that lets a caller
//     interrupt an in-flight generation yet keep the text already produced: the
//     returned Response is the best-effort partial, and the error tells the
//     caller it was cut short. On a clean end-of-stream the returned error is
//     nil and the Response is complete. Any other transport/decode failure is
//     returned as a non-context error with a zero or partial Response, exactly as
//     Complete surfaces faults.
type Streamer interface {
	// Stream sends one request and assembles the same Response that Complete
	// would, forwarding each output-text delta to onChunk as it arrives. It
	// honors ctx: on cancellation it returns the partial Response plus ctx.Err().
	Stream(ctx context.Context, system string, msgs []Message, tools []Tool, maxTokens int, onChunk func(Chunk)) (Response, error)
}
