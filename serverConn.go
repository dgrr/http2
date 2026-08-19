package http2

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// timerDisarmed is far enough in the future that a timer created with it will
// not fire before it gets Reset to a real interval.
const timerDisarmed = time.Duration(math.MaxInt64)

// errConnClosed signals that the read loop terminated the connection on
// purpose, typically after sending a connection-level GOAWAY. It is handled
// as a graceful shutdown rather than a transport error.
// closedStrmsCap is how many recently closed stream ids a connection keeps.
const closedStrmsCap = 256

// writeDrainTimeout is how long teardown waits for queued frames, a GOAWAY in
// particular, to reach a peer that may have stopped reading.
const writeDrainTimeout = time.Second

var errConnClosed = errors.New("connection closed after GOAWAY")

type connState int32

const (
	connStateOpen connState = iota
	connStateClosed
)

type serverConn struct {
	c net.Conn
	h fasthttp.RequestHandler

	br *bufio.Reader
	bw *bufio.Writer

	enc HPACK
	dec HPACK

	// last valid ID used as a reference for new IDs
	lastID uint32

	// client's window
	// should be int64 because the user can try to overflow it
	clientWindow int64

	// our values
	maxWindow     int32
	currentWindow int32

	writer chan *FrameHeader
	reader chan *FrameHeader

	// writeStop is closed when the connection is on its way out. Every send
	// into writer selects on it: the ping timer and the idle timer both queue
	// frames from their own goroutines, so closing writer to stop the write
	// loop meant one of them could panic the process with a send on a closed
	// channel.
	writeStop chan struct{}

	state connState
	// closeRef stores the last stream that was valid before sending a GOAWAY.
	// Thus, the number stored in closeRef is used to complete all the requests that were sent before
	// to gracefully close the connection with a GOAWAY.
	closeRef uint32

	// maxRequestTime is the max time of a request over one single stream
	maxHeaderList int
	// maxRequestBodySize mirrors fasthttp.Server.MaxRequestBodySize. The
	// request body is buffered in memory, so without a cap one stream can grow
	// until the process runs out.
	maxRequestBodySize int
	maxRequestTime     time.Duration
	pingInterval       time.Duration
	// maxIdleTime is the max time a client can be connected without sending any REQUEST.
	// As highlighted, PING/PONG frames are completely excluded.
	//
	// Therefore, a client that didn't send a request for more than `maxIdleTime` will see it's connection closed.
	maxIdleTime time.Duration

	st      Settings
	clientS Settings

	// pingTimer
	pingTimer       *time.Timer
	maxRequestTimer *time.Timer
	maxIdleTimer    *time.Timer

	closer chan struct{}

	debug  bool
	logger fasthttp.Logger
}

func (sc *serverConn) closeIdleConn() {
	sc.writeGoAway(0, NoError, "connection has been idle for a long time")
	if sc.debug {
		sc.logger.Printf("Connection is idle. Closing\n")
	}
	close(sc.closer)
}

func (sc *serverConn) Handshake() error {
	return Handshake(false, sc.bw, &sc.st, sc.maxWindow)
}

func (sc *serverConn) Serve() error {
	sc.closer = make(chan struct{}, 1)
	sc.writeStop = make(chan struct{})
	// Created disarmed. time.NewTimer(0) fires at once, and with no read
	// timeout configured every stream open at that moment looked overdue, so a
	// request that arrived in the same instant was answered with
	// RST_STREAM(CANCEL) for no reason anyone could see.
	sc.maxRequestTimer = time.NewTimer(timerDisarmed)
	// The connection-level flow-control window always starts at 65535 octets,
	// independent of SETTINGS_INITIAL_WINDOW_SIZE (RFC 7540 6.9.2).
	sc.clientWindow = int64(defaultWindowSize)

	if sc.maxIdleTime > 0 {
		sc.maxIdleTimer = time.AfterFunc(sc.maxIdleTime, sc.closeIdleConn)
	}

	// Create the ping timer here, before spawning the read/write goroutines, so
	// the field is written once and read (Stop) from other goroutines without a
	// data race. The writer channel is buffered, so an early tick does not block.
	//
	// sendPingAndSchedule rearms sc.pingTimer, so the callback must not be able
	// to run before the assignment lands: create the timer disarmed and arm it
	// afterwards.
	if sc.pingInterval > 0 {
		sc.pingTimer = time.AfterFunc(timerDisarmed, sc.sendPingAndSchedule)
		sc.pingTimer.Reset(sc.pingInterval)
	}

	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("Serve panicked: %s:\n%s\n", err, debug.Stack())
		}
	}()

	// writeDone lets the teardown wait for queued frames to reach the socket.
	writeDone := make(chan struct{})

	go func() {
		defer close(writeDone)

		// defer closing the connection in the writeLoop in case the writeLoop panics
		defer func() {
			_ = sc.c.Close()
		}()

		sc.writeLoop()
	}()

	go func() {
		sc.handleStreams()
		// Fix #55: The pingTimer fired while we were closing the connection.
		// It is nil when pings are disabled with a negative PingInterval.
		if sc.pingTimer != nil {
			sc.pingTimer.Stop()
		}
		// Tell the write loop to drain what is queued and stop. The channel
		// itself is never closed: anything still holding a frame would panic
		// trying to hand it over.
		close(sc.writeStop)
	}()

	defer func() {
		// close the reader here so we can stop handling stream updates
		close(sc.reader)

		// ServeConn closes the socket the moment this returns, so a GOAWAY that
		// is still queued would never reach the peer and it would see a bare
		// disconnect instead of an error code (RFC 7540 5.4.1). Closing the
		// reader unwinds handleStreams, which closes the writer, which lets the
		// write loop drain.
		//
		// Bounded, because a peer that has stopped reading would otherwise hold
		// this goroutine open for as long as it likes.
		select {
		case <-writeDone:
		case <-time.After(writeDrainTimeout):
		}
	}()

	var err error

	// unset any deadline
	if err = sc.c.SetWriteDeadline(time.Time{}); err == nil {
		err = sc.c.SetReadDeadline(time.Time{})
	}
	if err != nil {
		return err
	}

	err = sc.readLoop()
	if errors.Is(err, io.EOF) || errors.Is(err, errConnClosed) {
		// errConnClosed means we deliberately terminated the connection
		// after emitting a GOAWAY. It is not a transport failure.
		err = nil
	}

	sc.close()

	return err
}

func (sc *serverConn) close() {
	if sc.pingTimer != nil {
		sc.pingTimer.Stop()
	}

	if sc.maxIdleTimer != nil {
		sc.maxIdleTimer.Stop()
	}

	sc.maxRequestTimer.Stop()
}

func (sc *serverConn) handlePing(ping *Ping) {
	// ping belongs to the frame header the read loop releases as soon as this
	// returns, so echo the payload on a frame of our own. Forwarding the
	// borrowed one puts the same Ping in the pool twice, and two connections
	// then get handed the same object.
	ack := AcquireFrame(FramePing).(*Ping)
	ack.SetAck(true)
	ack.SetData(ping.Data())

	fr := AcquireFrameHeader()
	fr.SetBody(ack)

	sc.write(fr)
}

func (sc *serverConn) writePing() {
	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	sc.write(fr)
}

func (sc *serverConn) checkFrameWithStream(fr *FrameHeader) error {
	if fr.Stream()&1 == 0 {
		return NewGoAwayError(ProtocolError, "invalid stream id")
	}

	switch fr.Type() {
	case FramePing:
		return NewGoAwayError(ProtocolError, "ping is carrying a stream id")
	case FramePushPromise:
		return NewGoAwayError(ProtocolError, "clients can't send push_promise frames")
	}

	return nil
}

func (sc *serverConn) readLoop() (err error) {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("readLoop panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var fr *FrameHeader

	// expectContinuation holds the stream id of a header block that has been
	// opened by a HEADERS frame without END_HEADERS. While it is non-zero the
	// only frame the peer may send is a CONTINUATION on that same stream.
	// https://httpwg.org/specs/rfc7540.html#rfc.section.6.10
	var expectContinuation uint32

	for err == nil {
		fr, err = ReadFrameFromWithSize(sc.br, sc.clientS.frameSize)
		if err != nil {
			if errors.Is(err, ErrUnknownFrameType) {
				// Unknown frame types are discarded, not rejected (RFC 7540
				// 4.1). The exception is one appearing inside a header block,
				// which 6.10 makes a connection error.
				if expectContinuation != 0 {
					sc.writeGoAway(0, ProtocolError, "extension frame inside a header block")
					return errConnClosed
				}

				err = nil

				continue
			}

			// a malformed frame (wrong size, bad padding, ...) is a
			// connection error: emit the GOAWAY and close the connection.
			var h2err Error
			if errors.As(err, &h2err) && h2err.frameType == FrameGoAway {
				sc.writeGoAway(0, h2err.Code(), h2err.Error())
				return errConnClosed
			}

			break
		}

		// Enforce the CONTINUATION rules before any other handling. A header
		// block must be a single HEADERS/PUSH_PROMISE followed by zero or more
		// CONTINUATION frames on the same stream, with nothing interleaved.
		if expectContinuation != 0 {
			if fr.Type() != FrameContinuation || fr.Stream() != expectContinuation {
				sc.writeGoAway(0, ProtocolError, "expected a CONTINUATION frame")
				ReleaseFrameHeader(fr)
				return errConnClosed
			}

			if fr.Flags().Has(FlagEndHeaders) {
				expectContinuation = 0
			}
		} else if fr.Type() == FrameContinuation {
			sc.writeGoAway(0, ProtocolError, "unexpected CONTINUATION frame")
			ReleaseFrameHeader(fr)
			return errConnClosed
		} else if fr.Type() == FrameHeaders && !fr.Flags().Has(FlagEndHeaders) {
			expectContinuation = fr.Stream()
		}

		if fr.Stream() != 0 {
			if cerr := sc.checkFrameWithStream(fr); cerr != nil {
				// a frame that violates connection-level rules is a
				// connection error: emit the GOAWAY and terminate.
				sc.writeError(nil, cerr)
				ReleaseFrameHeader(fr)
				return errConnClosed
			}

			sc.reader <- fr
			continue
		}

		// handle 'anonymous' frames (frames without stream_id)
		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if it has ack, just ignore
				sc.handleSettings(st)
				// forward to handleStreams so the INITIAL_WINDOW_SIZE delta is
				// applied to open streams in frame order.
				sc.reader <- fr
				continue
			}
		case FrameWindowUpdate:
			win := int64(fr.Body().(*WindowUpdate).Increment())
			if win == 0 {
				sc.writeGoAway(0, ProtocolError, "window increment of 0")
				ReleaseFrameHeader(fr)
				return errConnClosed
			}

			// the actual window bookkeeping happens in handleStreams.
			sc.reader <- fr
			continue
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				sc.handlePing(ping)
			}
		case FrameGoAway:
			ga := fr.Body().(*GoAway)
			if ga.Code() == NoError {
				err = io.EOF
			} else {
				err = fmt.Errorf("goaway: %s: %s", ga.Code(), ga.Data())
			}
		default:
			sc.writeGoAway(0, ProtocolError, "invalid frame")
			ReleaseFrameHeader(fr)
			return errConnClosed
		}

		ReleaseFrameHeader(fr)
	}

	return err
}

// handleStreams handles everything related to the streams
// and the HPACK table is accessed synchronously.
func (sc *serverConn) handleStreams() {
	defer func() {
		if err := recover(); err != nil {
			sc.logger.Printf("handleStreams panicked: %s\n%s\n", err, debug.Stack())
		}
	}()

	var strms Streams
	var reqTimerArmed bool
	var openStreams int

	// curInitialWindow tracks the client's SETTINGS_INITIAL_WINDOW_SIZE, which
	// is the send window every new stream starts with. It starts at the spec
	// default of 65535; the client's SETTINGS frames are forwarded to this
	// goroutine, which applies any change as a delta to all open streams
	// (RFC 7540 6.9.2). Reading it here rather than sc.clientS keeps all
	// window state owned by this single goroutine.
	curInitialWindow := int32(defaultWindowSize)

	// closedStrms remembers recently closed stream ids so that a late frame on
	// one can be told apart from a frame on a stream that was never opened,
	// which is a protocol error rather than something to ignore. Only the most
	// recent ids are kept: a peer that has not caught up is at most a round
	// trip behind, and an unbounded set would grow for the whole life of the
	// connection.
	closedStrms := make(map[uint32]struct{}, closedStrmsCap)
	closedRing := make([]uint32, 0, closedStrmsCap)
	closedOldest := 0

	markClosed := func(id uint32) {
		if _, ok := closedStrms[id]; ok {
			return
		}

		if len(closedRing) < closedStrmsCap {
			closedRing = append(closedRing, id)
		} else {
			delete(closedStrms, closedRing[closedOldest])
			closedRing[closedOldest] = id
			closedOldest = (closedOldest + 1) % closedStrmsCap
		}

		closedStrms[id] = struct{}{}
	}

	closeStream := func(strm *Stream) {
		if strm.origType == FrameHeaders {
			openStreams--
		}

		strmID := strm.ID()

		markClosed(strmID)
		strms.Del(strm.ID())

		sc.closeBodyStream(strm)

		ctxPool.Put(strm.ctx)
		streamPool.Put(strm)

		if sc.debug {
			sc.logger.Printf("Stream destroyed %d. Open streams: %d\n", strmID, openStreams)
		}
	}

	// Frames come off sc.reader owned by this goroutine and have to go back to
	// the pool once the iteration that handled them is done. Releasing at the
	// top of the next iteration covers every path out of the body, of which
	// there are many, without a release on each one.
	var handled *FrameHeader

	releaseHandled := func() {
		if handled != nil {
			ReleaseFrameHeader(handled)
			handled = nil
		}
	}

	defer releaseHandled()

loop:
	for {
		releaseHandled()

		select {
		case <-sc.closer:
			break loop
		case <-sc.maxRequestTimer.C:
			reqTimerArmed = false

			// No read timeout configured means requests do not time out.
			if sc.maxRequestTime <= 0 {
				continue
			}

			deleteUntil := 0
			for _, strm := range strms {
				// the request is due if the startedAt time + maxRequestTime is in the past
				isDue := time.Now().After(
					strm.startedAt.Add(sc.maxRequestTime))
				if !isDue {
					break
				}

				deleteUntil++
			}

			for deleteUntil > 0 {
				strm := strms[0]

				if sc.debug {
					sc.logger.Printf("Stream timed out: %d\n", strm.ID())
				}
				sc.writeReset(strm.ID(), StreamCanceled)

				// set the state to closed in case it comes back to life later
				strm.SetState(StreamStateClosed)
				closeStream(strm)

				deleteUntil--
			}

			if len(strms) != 0 && sc.maxRequestTime > 0 {
				// the first in the stream list might have started with a PushPromise
				strm := strms.GetFirstOf(FrameHeaders)
				if strm != nil {
					reqTimerArmed = true
					// try to arm the timer
					when := time.Until(strm.startedAt.Add(sc.maxRequestTime))
					// if the time is negative or zero it triggers imm
					sc.maxRequestTimer.Reset(when)

					if sc.debug {
						sc.logger.Printf("Next request will timeout in %f seconds\n", when.Seconds())
					}
				}
			}
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			handled = fr

			// Connection-level flow-control frames are forwarded here so the
			// window bookkeeping and the resumption of buffered response data
			// happen in this single goroutine, in frame order.
			if fr.Stream() == 0 {
				switch fr.Type() {
				case FrameSettings:
					st := fr.Body().(*Settings)
					if st.hasWindowSize {
						delta := int64(int32(st.windowSize)) - int64(curInitialWindow)
						curInitialWindow = int32(st.windowSize)

						for _, s := range strms {
							s.window += delta
							if s.window > 1<<31-1 {
								sc.writeGoAway(0, FlowControlError, "stream flow-control window exceeded maximum")
								break loop
							}
						}

						sc.flushStreams(strms, closeStream)
					}
				case FrameWindowUpdate:
					sc.clientWindow += int64(fr.Body().(*WindowUpdate).Increment())
					if sc.clientWindow > 1<<31-1 {
						sc.writeGoAway(0, FlowControlError, "connection flow-control window exceeded maximum")
						break loop
					}

					sc.flushStreams(strms, closeStream)
				}

				continue
			}

			isClosing := atomic.LoadInt32((*int32)(&sc.state)) == int32(connStateClosed)

			var strm *Stream
			if fr.Stream() <= sc.lastID {
				strm = strms.Search(fr.Stream())
			}

			if strm == nil {
				// if the stream doesn't exist, create it

				if fr.Type() == FrameResetStream {
					// only send go away on idle stream not on an already-closed stream
					if fr.Stream() > sc.lastID {
						sc.writeGoAway(fr.Stream(), ProtocolError, "RST_STREAM on idle stream")
					}

					continue
				}

				if _, ok := closedStrms[fr.Stream()]; ok {
					// A WINDOW_UPDATE, RST_STREAM or PRIORITY frame may
					// legitimately arrive shortly after a stream is closed,
					// because the peer had not yet processed the END_STREAM or
					// RST_STREAM when it sent them. These MUST be ignored, not
					// treated as connection errors (RFC 7540 5.1). Anything else
					// (HEADERS, DATA, CONTINUATION) on a closed stream is an
					// error.
					switch fr.Type() {
					case FramePriority, FrameWindowUpdate, FrameResetStream:
					default:
						sc.writeGoAway(fr.Stream(), StreamClosedError, "frame on closed stream")
					}

					continue
				}

				// if the client has more open streams than the maximum allowed OR
				//   the connection is closing, then refuse the stream
				if openStreams >= int(sc.st.maxStreams) || isClosing {
					if sc.debug {
						if isClosing {
							sc.logger.Printf("Closing the connection. Rejecting stream %d\n", fr.Stream())
						} else {
							sc.logger.Printf("Max open streams reached: %d >= %d\n",
								openStreams, sc.st.maxStreams)
						}
					}

					sc.writeReset(fr.Stream(), RefusedStreamError)

					continue
				}

				if fr.Stream() < sc.lastID {
					sc.writeGoAway(fr.Stream(), ProtocolError, "stream ID is lower than the latest")
					continue
				}

				strm = NewStream(fr.Stream(), curInitialWindow)
				strms = append(strms, strm)

				// RFC(5.1.1):
				//
				// The identifier of a newly established stream MUST be numerically
				// greater than all streams that the initiating endpoint has opened
				// or reserved. This governs streams that are opened using a
				// HEADERS frame and streams that are reserved using PUSH_PROMISE.
				if fr.Type() == FrameHeaders {
					openStreams++
					sc.lastID = fr.Stream()
				}

				sc.createStream(sc.c, fr.Type(), strm)

				if sc.debug {
					sc.logger.Printf("Stream %d created. Open streams: %d\n", strm.ID(), openStreams)
				}

				if !reqTimerArmed && sc.maxRequestTime > 0 {
					reqTimerArmed = true
					sc.maxRequestTimer.Reset(sc.maxRequestTime)

					if sc.debug {
						sc.logger.Printf("Next request will timeout in %f seconds\n", sc.maxRequestTime.Seconds())
					}
				}
			}

			// if we have more than one stream (this one newly created) check if the previous finished sending the headers
			if fr.Type() == FrameHeaders {
				nstrm := strms.getPrevious(FrameHeaders)
				if nstrm != nil && !nstrm.headersFinished {
					sc.writeError(nstrm, NewGoAwayError(ProtocolError, "previous stream headers not ended"))
					continue
				}

				for len(strms) != 0 {
					nstrm := strms[0]
					// RFC(5.1.1):
					//
					// The first use of a new stream identifier implicitly
					// closes all streams in the "idle" state that might
					// have been initiated by that peer with a lower-valued stream identifier
					if nstrm.ID() < strm.ID() &&
						nstrm.State() == StreamStateIdle &&
						nstrm.origType == FrameHeaders {

						nstrm.SetState(StreamStateClosed)
						// nstrm, not strm: closing the stream that was just
						// created leaves nstrm at the head of the list, still
						// idle, so the loop never makes progress.
						closeStream(nstrm)

						if sc.debug {
							sc.logger.Printf("Canceling stream in idle state: %d\n", nstrm.ID())
						}

						sc.writeReset(nstrm.ID(), StreamCanceled)

						continue
					}

					break
				}

				if sc.maxIdleTimer != nil {
					sc.maxIdleTimer.Reset(sc.maxIdleTime)
				}
			}

			if err := sc.handleFrame(strm, fr); err != nil {
				sc.writeError(strm, err)
				strm.SetState(StreamStateClosed)

				// A GOAWAY carries a connection error, so stop serving this
				// connection rather than letting the peer carry on sending.
				// writeError has already queued the frame and the writer is
				// drained before it closes (RFC 7540 6.8).
				var connErr Error
				if errors.As(err, &connErr) &&
					connErr.frameType == FrameGoAway && connErr.Code() != NoError {
					break loop
				}
			}

			handleState(fr, strm)

			// Generate the response once the client is done sending the
			// request, then (re)send buffered response data. The response may
			// not fit the flow-control window in one go, in which case the
			// stream stays open until a WINDOW_UPDATE lets us finish.
			if strm.State() == StreamStateHalfClosed && !strm.responded {
				strm.responded = true

				// The declared content-length must match the number of DATA
				// bytes actually received.
				// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.6
				if strm.hasContentLength && strm.recvBody != strm.contentLength {
					sc.writeReset(strm.ID(), ProtocolError)
					strm.SetState(StreamStateClosed)
				} else if sc.handleEndRequest(strm) {
					strm.SetState(StreamStateClosed)
				}
			} else if strm.responded && strm.hasMoreToSend() {
				// a stream-level WINDOW_UPDATE may have opened up space
				if sc.sendData(strm) {
					strm.SetState(StreamStateClosed)
				}
			}

			if strm.State() == StreamStateClosed {
				closeStream(strm)
			}

			if isClosing {
				ref := atomic.LoadUint32(&sc.closeRef)
				// if there's no reference, then just close the connection
				if ref == 0 {
					break
				}

				// if we have a ref, then check that all streams previous to that ref are closed
				for _, strm := range strms {
					// if the stream is here, then it's not closed yet
					if strm.origType == FrameHeaders && strm.ID() <= ref {
						continue loop
					}
				}

				break loop
			}
		}
	}
}

// consumeRecvWindow accounts for DATA that has been received and hands the
// space straight back with WINDOW_UPDATE. Without it the peer's send window
// runs down and never recovers, so a client that respects flow control stops
// uploading for good once it has sent the initial window.
//
// The space is returned as soon as the bytes are buffered rather than when the
// handler reads them, so the receive window never becomes the thing that limits
// throughput. What bounds the memory instead is MaxRequestBodySize per stream
// and SETTINGS_MAX_CONCURRENT_STREAMS across the connection, plus the backlog
// on sc.reader, which stops being read once it is full and lets TCP push back.
// https://httpwg.org/specs/rfc7540.html#rfc.section.6.9
func (sc *serverConn) consumeRecvWindow(strm *Stream, fr *FrameHeader, n int) {
	if n <= 0 {
		return
	}

	// The body has already been copied into the request, so the stream window
	// goes back in full. There is nothing to give back on a stream the peer has
	// just finished with.
	if !fr.Flags().Has(FlagEndStream) {
		sc.writeWindowUpdate(strm.ID(), n)
	}

	sc.currentWindow -= int32(n)
	if sc.currentWindow < sc.maxWindow/2 {
		inc := sc.maxWindow - sc.currentWindow
		sc.currentWindow = sc.maxWindow

		sc.writeWindowUpdate(0, int(inc))
	}
}

func (sc *serverConn) writeWindowUpdate(id uint32, inc int) {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(inc)

	fr.SetBody(wu)

	sc.write(fr)
}

func (sc *serverConn) writeReset(strm uint32, code ErrorCode) {
	r := AcquireFrame(FrameResetStream).(*RstStream)

	fr := AcquireFrameHeader()
	fr.SetStream(strm)
	fr.SetBody(r)

	r.SetCode(code)

	sc.write(fr)

	if sc.debug {
		sc.logger.Printf(
			"%s: Reset(stream=%d, code=%s)\n",
			sc.c.RemoteAddr(), strm, code,
		)
	}
}

func (sc *serverConn) writeGoAway(strm uint32, code ErrorCode, message string) {
	ga := AcquireFrame(FrameGoAway).(*GoAway)

	fr := AcquireFrameHeader()

	ga.SetStream(strm)
	ga.SetCode(code)
	ga.SetData([]byte(message))

	fr.SetBody(ga)

	sc.write(fr)

	if strm != 0 {
		atomic.StoreUint32(&sc.closeRef, sc.lastID)
	}

	atomic.StoreInt32((*int32)(&sc.state), int32(connStateClosed))

	if sc.debug {
		sc.logger.Printf(
			"%s: GoAway(stream=%d, code=%s): %s\n",
			sc.c.RemoteAddr(), strm, code, message,
		)
	}
}

func (sc *serverConn) writeError(strm *Stream, err error) {
	streamErr := Error{}

	if !errors.As(err, &streamErr) {
		// Not one of ours, so there is no code to report. Without a stream
		// there is nothing to reset either, and the connection error path is
		// the only thing left.
		if strm == nil {
			sc.writeGoAway(0, InternalError, err.Error())
			return
		}

		sc.writeReset(strm.ID(), InternalError)
		strm.SetState(StreamStateClosed)

		return
	}

	switch streamErr.frameType {
	case FrameGoAway:
		if strm == nil {
			sc.writeGoAway(0, streamErr.Code(), streamErr.Error())
		} else {
			sc.writeGoAway(strm.ID(), streamErr.Code(), streamErr.Error())
		}
	case FrameResetStream:
		if strm == nil {
			sc.writeGoAway(0, streamErr.Code(), streamErr.Error())
			return
		}

		sc.writeReset(strm.ID(), streamErr.Code())
	}

	if strm != nil {
		strm.SetState(StreamStateClosed)
	}
}

func handleState(fr *FrameHeader, strm *Stream) {
	if fr.Type() == FrameResetStream {
		strm.SetState(StreamStateClosed)
	}

	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() == FrameHeaders {
			strm.SetState(StreamStateOpen)
			if fr.Flags().Has(FlagEndStream) {
				strm.SetState(StreamStateHalfClosed)
			}
		} // TODO: else push promise ...
	case StreamStateReserved:
		// TODO: ...
	case StreamStateOpen:
		if fr.Flags().Has(FlagEndStream) {
			strm.SetState(StreamStateHalfClosed)
		} else if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateHalfClosed:
		// a stream can only go from HalfClosed to Closed if the client
		// sends a ResetStream frame.
		if fr.Type() == FrameResetStream {
			strm.SetState(StreamStateClosed)
		}
	case StreamStateClosed:
	}
}

var logger = log.New(os.Stdout, "[HTTP/2] ", log.LstdFlags)

var ctxPool = sync.Pool{
	New: func() interface{} {
		return &fasthttp.RequestCtx{}
	},
}

func (sc *serverConn) createStream(c net.Conn, frameType FrameType, strm *Stream) {
	ctx := ctxPool.Get().(*fasthttp.RequestCtx)
	ctx.Request.Reset()
	ctx.Response.Reset()

	ctx.Init2(c, sc.logger, false)

	strm.origType = frameType
	strm.startedAt = time.Now()
	strm.SetData(ctx)
}

func (sc *serverConn) handleFrame(strm *Stream, fr *FrameHeader) error {
	err := sc.verifyState(strm, fr)
	if err != nil {
		return err
	}

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(ProtocolError, "received headers on a finished stream")
		}

		err = sc.handleHeaderFrame(strm, fr)
		if err != nil {
			return err
		}

		if fr.Flags().Has(FlagEndHeaders) {
			// headers are only finished if there's no previousHeaderBytes
			strm.headersFinished = len(strm.previousHeaderBytes) == 0
			if !strm.headersFinished {
				return NewGoAwayError(ProtocolError, "END_HEADERS received on an incomplete stream")
			}

			if err := validateRequestPseudoHeaders(strm); err != nil {
				return err
			}

			// calling req.URI() triggers a URL parsing, so because of that we need to delay the URL parsing.
			strm.ctx.Request.URI().SetSchemeBytes(strm.scheme)
		}
	case FrameData:
		if !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "stream didn't end the headers")
		}

		if strm.State() >= StreamStateHalfClosed {
			return NewGoAwayError(StreamClosedError, "stream closed")
		}

		data := fr.Body().(*Data).Data()
		strm.recvBody += len(data)

		if sc.maxRequestBodySize > 0 && strm.recvBody > sc.maxRequestBodySize {
			return NewResetStreamError(EnhanceYourCalm, "request body is too large")
		}

		strm.ctx.Request.AppendBody(data)

		sc.consumeRecvWindow(strm, fr, fr.Len())
	case FrameResetStream:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "RST_STREAM on idle stream")
		}
	case FramePriority:
		if strm.State() != StreamStateIdle && !strm.headersFinished {
			return NewGoAwayError(ProtocolError, "frame priority on an open stream")
		}

		if priorityFrame, ok := fr.Body().(*Priority); ok && priorityFrame.Stream() == strm.ID() {
			return NewGoAwayError(ProtocolError, "stream that depends on itself")
		}
	case FrameWindowUpdate:
		if strm.State() == StreamStateIdle {
			return NewGoAwayError(ProtocolError, "window update on idle stream")
		}

		win := int64(fr.Body().(*WindowUpdate).Increment())
		if win == 0 {
			return NewGoAwayError(ProtocolError, "window increment of 0")
		}

		if atomic.AddInt64(&strm.window, win) >= 1<<31-1 {
			return NewResetStreamError(FlowControlError, "window is above limits")
		}
	default:
		return NewGoAwayError(ProtocolError, "invalid frame")
	}

	return err
}

func (sc *serverConn) handleHeaderFrame(strm *Stream, fr *FrameHeader) error {
	// A second header block on a stream whose request headers are already done
	// is a trailer, which must carry both END_STREAM and END_HEADERS. Its
	// fields join the request headers, which is the nearest thing fasthttp's
	// request has to a place for them.
	// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1
	if strm.headersFinished && !fr.Flags().Has(FlagEndStream|FlagEndHeaders) {
		return NewGoAwayError(ProtocolError, "stream not open")
	}

	if headerFrame, ok := fr.Body().(*Headers); ok && headerFrame.Stream() == strm.ID() {
		return NewGoAwayError(ProtocolError, "stream that depends on itself")
	}

	// Only a HEADERS or PUSH_PROMISE frame opens a header block, and only when
	// there is nothing left over from a frame that cut a field in half.
	blockStart := fr.Type() != FrameContinuation && len(strm.previousHeaderBytes) == 0

	// Appending to the stream's own buffer and handing it back keeps the
	// capacity across frames instead of allocating a header block every time.
	b := append(strm.previousHeaderBytes, fr.Body().(FrameWithHeaders).Headers()...)
	strm.previousHeaderBytes = b[:0]

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	req := &strm.ctx.Request

	var err error

	fieldsProcessed := 0

	for len(b) > 0 {
		pb := b

		b, err = sc.dec.nextField(hf, blockStart, fieldsProcessed, b)
		if err != nil {
			// ErrUnexpectedSize means a header field spills past the bytes we
			// currently have. That is only legal when more frames are coming:
			// a HEADERS frame without END_HEADERS to be completed by a
			// CONTINUATION. If END_HEADERS is set, the block is complete and a
			// truncated field is a decoding error.
			if errors.Is(err, ErrUnexpectedSize) && len(pb) > 0 && !fr.Flags().Has(FlagEndHeaders) {
				err = nil
				strm.previousHeaderBytes = append(strm.previousHeaderBytes, pb...)
			} else {
				err = NewGoAwayError(CompressionError, err.Error())
			}

			break
		}

		k, v := hf.KeyBytes(), hf.ValueBytes()

		// RFC 7540 6.5.2 sizes a field as name + value + 32. The running total
		// spans the whole header block, so splitting it over CONTINUATION
		// frames does not get around the limit.
		strm.headerListSize += len(k) + len(v) + 32
		if sc.maxHeaderList > 0 && strm.headerListSize > sc.maxHeaderList {
			return NewGoAwayError(EnhanceYourCalm, "header list exceeds the maximum size")
		}

		// Header field names must not contain uppercase characters.
		// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2
		if hasUpperCase(k) {
			return NewResetStreamError(ProtocolError, "header field name contains uppercase characters")
		}

		if hf.IsPseudo() {
			// All pseudo-header fields must appear before regular header fields.
			// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.1
			if strm.regularSeen {
				return NewResetStreamError(ProtocolError, "pseudo-header field after regular header field")
			}

			switch {
			case bytes.Equal(k, StringMethod):
				if strm.pseudoMethod {
					return NewResetStreamError(ProtocolError, "duplicate :method pseudo-header")
				}
				strm.pseudoMethod = true
				req.Header.SetMethodBytes(v)
			case bytes.Equal(k, StringPath):
				if strm.pseudoPath {
					return NewResetStreamError(ProtocolError, "duplicate :path pseudo-header")
				}
				strm.pseudoPath = true
				strm.path = append(strm.path[:0], v...)
				req.Header.SetRequestURIBytes(v)
			case bytes.Equal(k, StringScheme):
				if strm.pseudoScheme {
					return NewResetStreamError(ProtocolError, "duplicate :scheme pseudo-header")
				}
				strm.pseudoScheme = true
				strm.scheme = append(strm.scheme[:0], v...)
			case bytes.Equal(k, StringAuthority):
				if strm.pseudoAuthority {
					return NewResetStreamError(ProtocolError, "duplicate :authority pseudo-header")
				}
				strm.pseudoAuthority = true
				req.Header.SetHostBytes(v)
				req.Header.AddBytesV("Host", v)
			default:
				// Any pseudo-header that is not a valid request pseudo-header
				// (including response pseudo-headers such as :status) is invalid.
				return NewResetStreamError(ProtocolError, fmt.Sprintf("invalid request pseudo-header %s", k))
			}

			fieldsProcessed++
			continue
		}

		// From here on it is a regular header field.
		strm.regularSeen = true

		// Connection-specific header fields are forbidden.
		// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.2
		if isConnectionSpecific(k) {
			return NewResetStreamError(ProtocolError, "connection-specific header field")
		}

		if bytes.Equal(k, StringTE) && !bytes.Equal(v, StringTrailers) {
			return NewResetStreamError(ProtocolError, "TE header field with a value other than trailers")
		}

		switch {
		case bytes.Equal(k, StringUserAgent):
			req.Header.SetUserAgentBytes(v)
		case bytes.Equal(k, StringContentType):
			req.Header.SetContentTypeBytes(v)
		case bytes.Equal(k, StringContentLength):
			if n, perr := parseUint(v); perr == nil {
				if sc.maxRequestBodySize > 0 && n > sc.maxRequestBodySize {
					return NewResetStreamError(EnhanceYourCalm, "request body is too large")
				}

				strm.contentLength = n
				strm.hasContentLength = true
			}
			req.Header.AddBytesKV(k, v)
		default:
			req.Header.AddBytesKV(k, v)
		}

		fieldsProcessed++
	}

	return err
}

// validateRequestPseudoHeaders enforces that a completed request header block
// carries the mandatory request pseudo-headers and a non-empty path.
// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.3
func validateRequestPseudoHeaders(strm *Stream) error {
	if !strm.pseudoMethod || !strm.pseudoScheme || !strm.pseudoPath {
		return NewResetStreamError(ProtocolError, "missing mandatory request pseudo-header")
	}

	// The :path pseudo-header must not be empty for http/https requests.
	// CONNECT and OPTIONS-asterisk requests are not handled here.
	if len(strm.path) == 0 {
		return NewResetStreamError(ProtocolError, "empty :path pseudo-header")
	}

	return nil
}

func (sc *serverConn) verifyState(strm *Stream, fr *FrameHeader) error {
	switch strm.State() {
	case StreamStateIdle:
		if fr.Type() != FrameHeaders && fr.Type() != FramePriority {
			return NewGoAwayError(ProtocolError, "wrong frame on idle stream")
		}
	case StreamStateHalfClosed:
		if fr.Type() != FrameWindowUpdate && fr.Type() != FramePriority && fr.Type() != FrameResetStream {
			return NewGoAwayError(StreamClosedError, "wrong frame on half-closed stream")
		}
	default:
	}

	return nil
}

// handleEndRequest dispatches the finished request to the handler and starts
// sending the response. It returns true when the whole response (including the
// END_STREAM flag) has been written, meaning the stream can be closed. It
// returns false when the response body is flow-control blocked and remains
// buffered in strm.pendingData until a WINDOW_UPDATE resumes it.
func (sc *serverConn) handleEndRequest(strm *Stream) bool {
	ctx := strm.ctx
	ctx.Request.Header.SetProtocolBytes(StringHTTP2)

	sc.h(ctx)

	hasBody := ctx.Response.IsBodyStream() || len(ctx.Response.Body()) > 0

	fr := AcquireFrameHeader()
	fr.SetStream(strm.ID())

	h := AcquireFrame(FrameHeaders).(*Headers)
	h.SetEndHeaders(true)
	h.SetEndStream(!hasBody)

	fr.SetBody(h)

	fasthttpResponseHeaders(h, &sc.enc, &ctx.Response)

	sc.write(fr)

	if !hasBody {
		return true
	}

	if ctx.Response.IsBodyStream() {
		// The body is pulled a frame at a time as the windows allow. Draining
		// it straight into the writer, which is what this used to do, put the
		// whole response on the wire regardless of what the peer had said it
		// could take.
		strm.bodyStream = ctx.Response.BodyStream()
		strm.bodySize = int64(ctx.Response.Header.ContentLength())
		strm.bodyRead = 0
		strm.pendingData = strm.pendingData[:0]
		strm.pendingEnd = false
	} else {
		// Buffer the body and send as much as the flow-control windows allow.
		strm.pendingData = append(strm.pendingData[:0], ctx.Response.Body()...)
		strm.pendingEnd = true
	}

	return sc.sendData(strm)
}

// refillPending pulls the next chunk of a streamed response body into the
// stream's buffer.
func (sc *serverConn) refillPending(strm *Stream) error {
	buf := copyBufPool.Get().(*[]byte)
	defer copyBufPool.Put(buf)

	n, err := strm.bodyStream.Read(*buf)
	if n > 0 {
		strm.pendingData = append(strm.pendingData[:0], (*buf)[:n]...)
		strm.bodyRead += int64(n)
	}

	switch {
	case errors.Is(err, io.EOF):
		strm.pendingEnd = true
	case err != nil:
		return err
	case n == 0:
		return errors.New("BUG: io.Reader returned 0, nil")
	}

	// A declared content length is the end of the body even if the reader has
	// not said so yet.
	if strm.bodySize >= 0 && strm.bodyRead >= strm.bodySize {
		strm.pendingEnd = true
	}

	return nil
}

// closeBodyStream releases a streamed response body. fasthttp hands out the
// reader, and whatever is behind it (a file, a pipe) stays open until this
// runs.
func (sc *serverConn) closeBodyStream(strm *Stream) {
	if strm.bodyStream == nil {
		return
	}

	strm.bodyStream = nil

	if strm.ctx != nil {
		_ = strm.ctx.Response.CloseBodyStream()
	}
}

// sendData writes as much of strm.pendingData as the connection and stream
// flow-control windows allow, splitting into frames no larger than the maximum
// frame size. It returns true once the entire buffer (and the END_STREAM flag)
// has been sent. A negative window simply blocks until a WINDOW_UPDATE arrives.
func (sc *serverConn) sendData(strm *Stream) bool {
	for {
		if len(strm.pendingData) == 0 {
			if strm.bodyStream == nil {
				break
			}

			if err := sc.refillPending(strm); err != nil {
				// The response cannot be finished, and the peer is part way
				// through a body it will otherwise wait for.
				sc.logger.Printf("ERROR: reading the response body: %s\n", err)
				sc.closeBodyStream(strm)
				sc.writeReset(strm.ID(), InternalError)

				return true
			}

			if len(strm.pendingData) == 0 {
				break
			}
		}

		avail := strm.window
		if sc.clientWindow < avail {
			avail = sc.clientWindow
		}

		if avail <= 0 {
			return false
		}

		step := int64(1 << 14) // max frame size 16384
		if avail < step {
			step = avail
		}
		if int64(len(strm.pendingData)) < step {
			step = int64(len(strm.pendingData))
		}

		chunk := strm.pendingData[:step]
		strm.pendingData = strm.pendingData[step:]
		end := strm.pendingEnd && len(strm.pendingData) == 0

		fr := AcquireFrameHeader()
		fr.SetStream(strm.ID())

		data := AcquireFrame(FrameData).(*Data)
		data.SetEndStream(end)
		data.SetPadding(false)
		data.SetData(chunk)

		fr.SetBody(data)

		sc.write(fr)

		strm.window -= step
		sc.clientWindow -= step
	}

	sc.closeBodyStream(strm)

	return true
}

// flushStreams resumes any streams whose buffered response data was blocked on
// flow control, closing the ones that finish. It is called after a
// connection-level window change.
func (sc *serverConn) flushStreams(strms Streams, closeStream func(*Stream)) {
	var done []*Stream

	for _, s := range strms {
		if s.responded && s.hasMoreToSend() && sc.sendData(s) {
			done = append(done, s)
		}
	}

	for _, s := range done {
		s.SetState(StreamStateClosed)
		closeStream(s)
	}
}

var (
	// Pointers, not slices: putting a slice back boxes it into an interface,
	// which is an allocation every time.
	copyBufPool = sync.Pool{
		New: func() interface{} {
			b := make([]byte, 1<<14) // max frame size 16384

			return &b
		},
	}
)

func (sc *serverConn) sendPingAndSchedule() {
	sc.writePing()

	sc.pingTimer.Reset(sc.pingInterval)
}

// write queues a frame for the peer. It drops the frame instead of blocking
// once the connection is on its way out: the ping and idle timers queue frames
// from their own goroutines and cannot know the write loop has gone.
func (sc *serverConn) write(fr *FrameHeader) {
	select {
	case sc.writer <- fr:
	case <-sc.writeStop:
		ReleaseFrameHeader(fr)
	}
}

func (sc *serverConn) writeLoop() {
	buffered := 0

	send := func(fr *FrameHeader) error {
		_, err := fr.WriteTo(sc.bw)
		if err == nil && (len(sc.writer) == 0 || buffered > 10) {
			err = sc.bw.Flush()
			buffered = 0
		} else if err == nil {
			buffered++
		}

		ReleaseFrameHeader(fr)

		if err != nil {
			sc.logger.Printf("ERROR: writeLoop: %s\n", err)
		}

		return err
	}

	for {
		select {
		case fr := <-sc.writer:
			if send(fr) != nil {
				return
			}
		case <-sc.writeStop:
			// Drain what is already queued: a GOAWAY written on the way out is
			// usually the last thing in here, and the peer needs to see it.
			for {
				select {
				case fr := <-sc.writer:
					if send(fr) != nil {
						return
					}
				default:
					_ = sc.bw.Flush()

					return
				}
			}
		}
	}
}

func (sc *serverConn) handleSettings(st *Settings) {
	st.CopyTo(&sc.clientS)
	sc.enc.SetMaxTableSize(sc.clientS.HeaderTableSize())

	// The per-stream send windows are adjusted in handleStreams, where the
	// stream table lives. The connection-level window is not affected by
	// SETTINGS_INITIAL_WINDOW_SIZE (RFC 7540 6.9.2).

	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	sc.write(fr)
}

func fasthttpResponseHeaders(dst *Headers, hp *HPACK, res *fasthttp.Response) {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	hf.SetKeyBytes(StringStatus)
	hf.SetValueBytes(statusBytes(res.Header.StatusCode()))

	dst.AppendHeaderField(hp, hf, true)

	if !res.IsBodyStream() {
		res.Header.SetContentLength(len(res.Body()))
	}
	// Remove the Connection field
	res.Header.Del("Connection")
	// Remove the Transfer-Encoding field
	res.Header.Del("Transfer-Encoding")

	for k, v := range res.Header.All() {
		// Lowercase hf's own copy of the name. k can point straight at
		// fasthttp's package level header name constants, which every
		// connection in the process shares.
		hf.SetBytes(k, v)
		ToLower(hf.key)

		dst.AppendHeaderField(hp, hf, false)
	}
}
