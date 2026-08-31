package http2

import (
	"io"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

type StreamState int8

const (
	StreamStateIdle StreamState = iota
	StreamStateReserved
	StreamStateOpen
	StreamStateHalfClosed
	StreamStateClosed
)

func (ss StreamState) String() string {
	switch ss {
	case StreamStateIdle:
		return "Idle"
	case StreamStateReserved:
		return "Reserved"
	case StreamStateOpen:
		return "Open"
	case StreamStateHalfClosed:
		return "HalfClosed"
	case StreamStateClosed:
		return "Closed"
	}

	return "IDK"
}

type Stream struct {
	// window is updated with a 64-bit atomic, so it has to stay first: on
	// 32-bit platforms only the first word of the struct is guaranteed to be
	// 8-byte aligned, and a misaligned 64-bit atomic panics at run time.
	// align32_test.go turns a move into a compile error.
	window              int64
	id                  uint32
	state               StreamState
	ctx                 *fasthttp.RequestCtx
	scheme              []byte
	path                []byte
	previousHeaderBytes []byte

	// request pseudo-header bookkeeping, used to enforce RFC 7540 8.1.2.
	pseudoMethod    bool
	pseudoScheme    bool
	pseudoPath      bool
	pseudoAuthority bool
	// regularSeen is set once a non-pseudo header is decoded in the block.
	// Any pseudo-header after that point is invalid.
	regularSeen bool

	// content-length declared by the request, and the number of DATA bytes
	// received so far, used to validate the two match (RFC 7540 8.1.2.6).
	contentLength    int
	hasContentLength bool
	recvBody         int

	// pendingData holds response body bytes that have not been sent yet
	// because the flow-control window is exhausted. pendingEnd records whether
	// END_STREAM must be set once pendingData is fully flushed.
	pendingData []byte
	pendingEnd  bool

	// bodyStream is a streamed response body, pulled from a frame at a time as
	// the windows allow rather than drained into the writer in one go.
	// bodySize is the declared content length, negative when it is unknown.
	// bodyBuf is where each chunk is read, and pendingData points into it.
	bodyStream io.Reader
	bodySize   int64
	bodyRead   int64
	bodyBuf    []byte
	// responded is set once the request has been dispatched to the handler, so
	// the half-closed handling does not run it twice.
	responded bool

	// handlerRunning is set while the handler goroutine owns ctx. The stream
	// cannot be recycled during that window: the pools would hand the same
	// RequestCtx to another stream while the handler is still writing to it.
	handlerRunning bool

	// abandoned is set when the stream was closed, by a reset or a timeout,
	// while its handler was still running. Whatever the handler produces is
	// dropped, and the stream is recycled when it reports back.
	abandoned bool

	// headerListSize is the running RFC 7540 6.5.2 size of the header block
	// being decoded, summed across the HEADERS frame and its CONTINUATIONs.
	headerListSize int

	// headerErr is a stream error found while decoding the header block, held
	// until the block ends so the rest of it still reaches the HPACK decoder.
	headerErr error

	// original type
	origType        FrameType
	startedAt       time.Time
	headersFinished bool
}

var streamPool = sync.Pool{
	New: func() interface{} {
		return &Stream{}
	},
}

func NewStream(id uint32, win int32) *Stream {
	strm := streamPool.Get().(*Stream)
	strm.id = id
	strm.window = int64(win)
	strm.state = StreamStateIdle
	strm.headersFinished = false
	strm.startedAt = time.Time{}
	strm.previousHeaderBytes = strm.previousHeaderBytes[:0]
	strm.ctx = nil
	// Appending a constant reuses the pooled buffer. Assigning []byte("https")
	// throws it away and allocates on every stream.
	strm.scheme = append(strm.scheme[:0], "https"...)
	strm.path = strm.path[:0]
	strm.pseudoMethod = false
	strm.pseudoScheme = false
	strm.pseudoPath = false
	strm.pseudoAuthority = false
	strm.regularSeen = false
	strm.contentLength = 0
	strm.hasContentLength = false
	strm.recvBody = 0
	// nil rather than [:0]: for a buffered response pendingData points straight
	// at the response body rather than at a buffer of the stream's own, and
	// appending to that would write into a response somebody else now owns.
	strm.pendingData = nil
	strm.pendingEnd = false
	strm.bodyStream = nil
	strm.bodySize = 0
	strm.bodyRead = 0
	strm.responded = false
	strm.handlerRunning = false
	strm.abandoned = false
	strm.origType = 0
	strm.headerListSize = 0
	strm.headerErr = nil

	return strm
}

func (s *Stream) ID() uint32 {
	return s.id
}

func (s *Stream) SetID(id uint32) {
	s.id = id
}

func (s *Stream) State() StreamState {
	return s.state
}

func (s *Stream) SetState(state StreamState) {
	s.state = state
}

func (s *Stream) Window() int32 {
	return int32(s.window)
}

func (s *Stream) SetWindow(win int32) {
	s.window = int64(win)
}

func (s *Stream) IncrWindow(win int32) {
	s.window += int64(win)
}

func (s *Stream) Ctx() *fasthttp.RequestCtx {
	return s.ctx
}

func (s *Stream) SetData(ctx *fasthttp.RequestCtx) {
	s.ctx = ctx
}

// hasMoreToSend reports whether the stream still owes the peer response data,
// either buffered or still to be pulled from a streamed body.
func (s *Stream) hasMoreToSend() bool {
	return len(s.pendingData) > 0 || s.bodyStream != nil
}

// continuingHeaders reports whether fr carries the rest of a header block this
// stream has already started.
//
// END_STREAM on the HEADERS frame half-closes the stream, but it ends the
// message body, not the header block: that is only over at END_HEADERS, so the
// CONTINUATION frames that finish it are still expected even though the stream
// is half-closed (RFC 7540 6.2, 6.10).
func (s *Stream) continuingHeaders(fr *FrameHeader) bool {
	return fr.Type() == FrameContinuation && !s.headersFinished
}
