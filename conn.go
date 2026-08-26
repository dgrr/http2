package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// ConnOpts defines the connection options.
type ConnOpts struct {
	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of <=0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled
	PingInterval time.Duration

	// DisablePingChecking ...
	DisablePingChecking bool

	// OnDisconnect is a callback that fires when the Conn disconnects.
	OnDisconnect func(c *Conn)
}

// Handshake performs an HTTP/2 handshake. That means, it will send
// the preface if `preface` is true, send a settings frame and a
// window update frame (for the connection's window).
// TODO: explain more
func Handshake(preface bool, bw *bufio.Writer, st *Settings, maxWin int32) error {
	if preface {
		err := WritePreface(bw)
		if err != nil {
			return err
		}
	}

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	// write the settings
	st2 := &Settings{}
	st.CopyTo(st2)

	fr.SetBody(st2)

	_, err := fr.WriteTo(bw)
	if err == nil {
		// then send a window update
		fr := AcquireFrameHeader()
		wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
		wu.SetIncrement(int(maxWin))

		fr.SetBody(wu)

		_, err = fr.WriteTo(bw)
		if err == nil {
			err = bw.Flush()
		}

		ReleaseFrameHeader(fr)
	}

	return err
}

// pendingBody is the tail of a request body that flow control has not let out
// yet. The write loop owns it; the read loop only grows its window.
type pendingBody struct {
	ctx    *Ctx
	body   []byte
	window int32

	// stream is a streamed request body, pulled a chunk at a time as the
	// windows allow rather than held in memory in one go. size is the declared
	// content length, negative when the reader runs until EOF, and buf is where
	// each chunk lands, with body pointing into it.
	stream io.Reader
	size   int64
	read   int64
	buf    []byte

	// drained is set once the reader has nothing left to give, so the last
	// chunk can carry END_STREAM.
	drained bool
}

// hasMore reports whether the body still owes the peer bytes.
func (pb *pendingBody) hasMore() bool {
	return len(pb.body) > 0 || (pb.stream != nil && !pb.drained)
}

// Conn represents a raw HTTP/2 connection over TLS + TCP.
type Conn struct {
	c net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	nextID uint32

	maxWindow     int32
	currentWindow int32

	openStreams int32

	// Flow control for the data we send. The read loop grows the windows as
	// WINDOW_UPDATE arrives, the write loop spends them.
	// https://httpwg.org/specs/rfc7540.html#rfc.section.6.9
	sendLck      sync.Mutex
	connWindow   int32
	streamWindow int32
	pending      map[uint32]*pendingBody

	// winCh wakes the write loop when a send window has opened.
	winCh chan struct{}

	// The server's settings are read on the write loop and on whatever
	// goroutine calls CanOpenStream, but a SETTINGS frame can arrive at any
	// point, so the values the other goroutines need are kept as atomics
	// rather than read straight out of serverS.
	maxStreams   uint32
	maxFrameSize uint32

	// encTableSize is the header table size the server last asked for. The
	// read loop records it and the write loop, which owns the encoder, applies
	// it. encTableSizeSeen belongs to the write loop alone.
	encTableSize     uint32
	encTableSizeSeen uint32

	current Settings

	// serverS belongs to the read loop once the handshake is over.
	serverS Settings

	state    connState
	closeRef uint32

	// goAway is set once the server has told us not to open more streams.
	goAway uint32

	// reqQueued maps a stream id to the request waiting on it. A plain map
	// under a mutex beats sync.Map here: every entry is written once and
	// deleted once, which is the pattern sync.Map is worst at.
	reqLck    sync.Mutex
	reqQueued map[uint32]*Ctx

	in  chan *Ctx
	out chan *FrameHeader

	// bwLck guards bw. Every other write to it happens on the write loop
	// goroutine, but Close can be called from any goroutine: the read loop on
	// its way out, or a caller of Client.Close.
	bwLck sync.Mutex

	pingInterval time.Duration

	// unacks counts pings sent without a matching ack. The write loop bumps it
	// and the read loop clears it, so it has to be touched atomically.
	unacks      int32
	disableAcks bool

	// lastErrLck guards lastErr. Both loops write it and LastErr reads it from
	// whatever goroutine the caller happens to be on.
	lastErrLck sync.Mutex
	lastErr    error

	onDisconnect func(*Conn)

	// done is closed by Close. Every send into in and out selects on it: once
	// the write loop is gone a bare send blocks forever, and closing in instead
	// would panic any Write that is running concurrently.
	done chan struct{}

	closed uint64
}

// setLastErr records the error that ended the connection, keeping the first one
// seen: it is the one that explains the rest.
func (c *Conn) setLastErr(err error) {
	if err == nil {
		return
	}

	c.lastErrLck.Lock()

	if c.lastErr == nil {
		c.lastErr = err
	}

	c.lastErrLck.Unlock()
}

// ErrConnectionClosed is returned for requests handed to a connection that has
// already been closed.
var ErrConnectionClosed = errors.New("connection is closed")

// closeErr returns the reason the connection ended, for resolving requests that
// never made it onto the wire.
func (c *Conn) closeErr() error {
	if err := c.LastErr(); err != nil {
		return err
	}

	return ErrConnectionClosed
}

// NewConn returns a new HTTP/2 connection.
// To start using the connection you need to call Handshake.
func NewConn(c net.Conn, opts ConnOpts) *Conn {
	nc := &Conn{
		c:             c,
		br:            bufio.NewReaderSize(c, 4096),
		bw:            bufio.NewWriterSize(c, maxFrameSize),
		enc:           AcquireHPACK(),
		dec:           AcquireHPACK(),
		nextID:        1,
		maxWindow:     1 << 20,
		currentWindow: 1 << 20,
		connWindow:    int32(defaultWindowSize),
		streamWindow:  int32(defaultWindowSize),
		maxStreams:    defaultConcurrentStreams,
		maxFrameSize:  defaultDataFrameSize,
		pending:       make(map[uint32]*pendingBody),
		reqQueued:     make(map[uint32]*Ctx),
		winCh:         make(chan struct{}, 1),
		in:            make(chan *Ctx, 128),
		out:           make(chan *FrameHeader, 128),
		done:          make(chan struct{}),
		pingInterval:  opts.PingInterval,
		disableAcks:   opts.DisablePingChecking,
		onDisconnect:  opts.OnDisconnect,
	}

	nc.current.SetMaxWindowSize(1 << 20)
	nc.current.SetPush(false)

	return nc
}

// queueReq records the request waiting on a stream.
func (c *Conn) queueReq(id uint32, ctx *Ctx) {
	c.reqLck.Lock()
	c.reqQueued[id] = ctx
	c.reqLck.Unlock()
}

// dequeueReq drops a stream from the table.
func (c *Conn) dequeueReq(id uint32) {
	c.reqLck.Lock()
	delete(c.reqQueued, id)
	c.reqLck.Unlock()
}

// takeReq drops a stream from the table and reports whether it was still
// there. Exactly one caller gets a true, which is what decides who accounts for
// the stream closing: a cancel and a late response can both reach the same
// stream, and counting it twice would leave the connection thinking it has
// fewer streams open than it does.
func (c *Conn) takeReq(id uint32) bool {
	c.reqLck.Lock()

	_, ok := c.reqQueued[id]
	if ok {
		delete(c.reqQueued, id)
	}

	c.reqLck.Unlock()

	return ok
}

// loadReq returns the request waiting on a stream, if there is one.
func (c *Conn) loadReq(id uint32) (*Ctx, bool) {
	c.reqLck.Lock()
	ctx, ok := c.reqQueued[id]
	c.reqLck.Unlock()

	return ctx, ok
}

// takeAllReqs empties the table and returns what was in it, for resolving
// everything at once when the connection ends.
func (c *Conn) takeAllReqs() []*Ctx {
	c.reqLck.Lock()
	defer c.reqLck.Unlock()

	if len(c.reqQueued) == 0 {
		return nil
	}

	out := make([]*Ctx, 0, len(c.reqQueued))
	for id, ctx := range c.reqQueued {
		out = append(out, ctx)

		delete(c.reqQueued, id)
	}

	return out
}

// Dialer allows creating HTTP/2 connections by specifying an address and tls configuration.
type Dialer struct {
	// Addr is the server's address in the form: `host:port`.
	Addr string

	// TLSConfig is the tls configuration.
	//
	// If TLSConfig is nil, a default one will be defined on the Dial call.
	TLSConfig *tls.Config

	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of 0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled.
	PingInterval time.Duration

	// NetDial defines the callback for establishing new connection to the host.
	// Default Dial is used if not set.
	NetDial fasthttp.DialFunc
}

func (d *Dialer) tryDial() (net.Conn, error) {
	if d.TLSConfig == nil || !func() bool {
		for _, proto := range d.TLSConfig.NextProtos {
			if proto == "h2" {
				return true
			}
		}

		return false
	}() {
		configureDialer(d)
	}

	var c net.Conn
	var err error

	if d.NetDial != nil {
		c, err = d.NetDial(d.Addr)
		if err != nil {
			return nil, err
		}
	} else {
		tcpAddr, err := net.ResolveTCPAddr("tcp", d.Addr)
		if err != nil {
			return nil, err
		}
		c, err = net.DialTCP("tcp", nil, tcpAddr)
		if err != nil {
			return nil, err
		}
	}

	tlsConn := tls.Client(c, d.TLSConfig)

	if err := tlsConn.Handshake(); err != nil {
		_ = c.Close()
		return nil, err
	}

	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		_ = c.Close()
		return nil, ErrServerSupport
	}

	return tlsConn, nil
}

// Dial creates an HTTP/2 connection or returns an error.
//
// An expected error is ErrServerSupport.
func (d *Dialer) Dial(opts ConnOpts) (*Conn, error) {
	c, err := d.tryDial()
	if err != nil {
		return nil, err
	}

	nc := NewConn(c, opts)

	err = nc.Handshake()
	return nc, err
}

// SetOnDisconnect sets the callback that will fire when the HTTP/2 connection is closed.
func (c *Conn) SetOnDisconnect(cb func(*Conn)) {
	c.onDisconnect = cb
}

// LastErr returns the last registered error in case the connection was closed by the server.
func (c *Conn) LastErr() error {
	c.lastErrLck.Lock()
	defer c.lastErrLck.Unlock()

	return c.lastErr
}

// Handshake will perform the necessary handshake to establish the connection
// with the server. If an error is returned you can assume the TCP connection has been closed.
func (c *Conn) Handshake() error {
	err := c.doHandshake()
	if err == nil {
		go c.writeLoop()
		go c.readLoop()
	}

	return err
}

func (c *Conn) doHandshake() error {
	var err error

	if err = Handshake(true, c.bw, &c.current, c.maxWindow-65535); err != nil {
		_ = c.c.Close()
		return err
	}

	var fr *FrameHeader

	if fr, err = ReadFrameFrom(c.br); err == nil && fr.Type() != FrameSettings {
		_ = c.c.Close()
		return fmt.Errorf("unexpected frame, expected settings, got %s", fr.Type())
	} else if err == nil {
		st := fr.Body().(*Settings)
		if !st.IsAck() {
			st.CopyTo(&c.serverS)

			// Nothing else is running yet, so these can be set directly.
			c.streamWindow = int32(c.serverS.MaxWindowSize())
			c.maxStreams = c.serverS.MaxConcurrentStreams()
			c.maxFrameSize = c.serverS.MaxFrameSize()

			if st.HeaderTableSize() <= defaultHeaderTableSize {
				c.enc.SetMaxTableSize(st.HeaderTableSize())
				c.encTableSize = st.HeaderTableSize()
				c.encTableSizeSeen = st.HeaderTableSize()
			}

			// reply back
			fr := AcquireFrameHeader()

			stRes := AcquireFrame(FrameSettings).(*Settings)
			stRes.SetAck(true)

			fr.SetBody(stRes)

			if _, err = fr.WriteTo(c.bw); err == nil {
				err = c.bw.Flush()
			}

			ReleaseFrameHeader(fr)
		}
	}

	if err != nil {
		_ = c.c.Close()
	} else {
		ReleaseFrameHeader(fr)
	}

	return err
}

// maxStreamID is the largest stream identifier the protocol allows. A client
// that runs out has to open a new connection.
// https://httpwg.org/specs/rfc7540.html#rfc.section.5.1.1
const maxStreamID = uint32(1)<<31 - 1

// CanOpenStream returns whether the client will be able to open a new stream or not.
//
// It reports false once the server has told us to stop (GOAWAY) and once the
// stream identifiers on this connection have run out, so the caller moves on to
// a fresh connection instead of writing frames the server will reject.
func (c *Conn) CanOpenStream() bool {
	if atomic.LoadUint32(&c.goAway) != 0 {
		return false
	}

	if atomic.LoadUint32(&c.nextID) > maxStreamID {
		return false
	}

	return atomic.LoadInt32(&c.openStreams) < int32(atomic.LoadUint32(&c.maxStreams))
}

// Closed indicates whether the connection is closed or not.
func (c *Conn) Closed() bool {
	return atomic.LoadUint64(&c.closed) == 1
}

// Close closes the connection gracefully, sending a GoAway message
// and then closing the underlying TCP connection.
func (c *Conn) Close() error {
	if !atomic.CompareAndSwapUint64(&c.closed, 0, 1) {
		return io.EOF
	}

	// in is deliberately not closed: Write can be running on any goroutine, and
	// a send on a closed channel panics. Closing done tells it to stop instead.
	close(c.done)

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	ga := AcquireFrame(FrameGoAway).(*GoAway)
	ga.SetStream(0)
	ga.SetCode(NoError)

	fr.SetBody(ga)

	c.bwLck.Lock()

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	c.bwLck.Unlock()

	_ = c.c.Close()

	if c.onDisconnect != nil {
		c.onDisconnect(c)
	}

	return err
}

// Write queues the request to be sent to the server.
//
// If the connection is closed before the request reaches the write loop, the
// Ctx is resolved with the reason instead of being left to time out.
func (c *Conn) Write(r *Ctx) {
	select {
	case c.in <- r:
	case <-c.done:
		r.resolve(c.closeErr())

		return
	}

	// The write loop may have gone away between the send and now, in which case
	// it has already drained the queue and nobody will ever pick this Ctx up.
	// Resolving twice is harmless: Err is buffered and read once.
	select {
	case <-c.done:
		r.resolve(c.closeErr())
	default:
	}
}

// writeOut queues a connection-level frame. It drops the frame rather than
// blocking forever when the write loop has already exited.
func (c *Conn) writeOut(fr *FrameHeader) {
	select {
	case c.out <- fr:
	case <-c.done:
		ReleaseFrameHeader(fr)
	}
}

var ErrStreamNotReady = errors.New("stream hasn't been created")

// ErrNoMoreStreamIDs is returned once a connection has used up the stream
// identifier space. The connection stays usable for the streams already on it,
// but no new request can be started: the caller needs a new connection.
var ErrNoMoreStreamIDs = errors.New("no more stream ids available on this connection")

// Cancel will try to cancel the request.
//
// Cancel can only return ErrStreamNotReady when the cancel is performed before the stream is created.
func (c *Conn) Cancel(ctx *Ctx) error {
	if atomic.LoadUint32(&ctx.streamID) == 0 {
		return ErrStreamNotReady
	}

	c.cancel(ctx)

	return nil
}

func (c *Conn) cancel(ctx *Ctx) {
	id := atomic.LoadUint32(&ctx.streamID)
	if id == 0 {
		// The request never reached the wire, so there is no stream to reset.
		// RST_STREAM on stream 0 is a connection error, not a no-op.
		return
	}

	// Whatever is left of the body is not going out on a stream we are
	// resetting, and the buffer stops being ours as soon as RoundTrip returns.
	c.deletePending(id)

	// Drop the stream here rather than waiting for a response that may never
	// come. Leaving it queued kept the Ctx alive for the life of the connection
	// and, worse, left openStreams counting a stream that was over, so enough
	// timed-out requests made the connection refuse to open any more.
	if c.takeReq(id) {
		atomic.AddInt32(&c.openStreams, -1)
	}

	c.cancelStream(id, StreamCanceled)
}

// cancelStream resets a stream that cannot be finished. The caller has already
// taken it off the queue.
func (c *Conn) cancelStream(id uint32, code ErrorCode) {
	h := AcquireFrameHeader()
	h.SetStream(id)

	fr := AcquireFrame(FrameResetStream).(*RstStream)
	fr.SetCode(code)

	h.SetBody(fr)

	c.writeOut(h)
}

type WriteError struct {
	err error
}

func (we WriteError) Error() string {
	return fmt.Sprintf("writing error: %s", we.err)
}

func (we WriteError) Unwrap() error {
	return we.err
}

func (we WriteError) Is(target error) bool {
	return errors.Is(we.err, target)
}

func (we WriteError) As(target interface{}) bool {
	return errors.As(we.err, target)
}

func (c *Conn) writeLoop() {
	lastErr := c.runWriteLoop()
	if lastErr == nil {
		lastErr = io.ErrUnexpectedEOF
	}

	c.setLastErr(lastErr)

	// Close before draining, not after. Closing is what stops Write from
	// handing us requests, so anything that lands in the queue from here on
	// sees a closed connection and resolves itself.
	_ = c.Close()

	for _, ctx := range c.takeAllReqs() {
		ctx.resolve(lastErr)
	}

	for {
		select {
		case ctx := <-c.in:
			ctx.resolve(lastErr)
		case fr := <-c.out:
			ReleaseFrameHeader(fr)
		default:
			return
		}
	}
}

func (c *Conn) runWriteLoop() (lastErr error) {
	defer func() {
		if err := recover(); err != nil {
			if lastErr == nil {
				switch errn := err.(type) {
				case error:
					lastErr = errn
				case string:
					lastErr = errors.New(errn)
				default:
					lastErr = fmt.Errorf("%v", errn)
				}
			}
		}
	}()

	if c.pingInterval <= 0 {
		c.pingInterval = DefaultPingInterval
	}

	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return lastErr
		case ctx := <-c.in: // sending requests
			err := c.writeRequest(ctx)
			if err != nil {
				ctx.resolve(err)

				if errors.Is(err, ErrNotAvailableStreams) {
					continue
				}

				return WriteError{err}
			}
		case fr := <-c.out: // generic output
			err := c.writeFrame(fr)

			ReleaseFrameHeader(fr)

			if err != nil {
				return WriteError{err}
			}
		case <-c.winCh: // a send window opened
			if err := c.flushPending(); err != nil {
				return WriteError{err}
			}
		case <-ticker.C: // ping
			if err := c.writePing(); err != nil {
				return WriteError{err}
			}
		}

		if !c.disableAcks && atomic.LoadInt32(&c.unacks) >= 3 {
			return ErrTimeout
		}
	}
}

func (c *Conn) writeFrame(fr *FrameHeader) error {
	c.bwLck.Lock()
	defer c.bwLck.Unlock()

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		if err = c.bw.Flush(); err != nil {
			return err
		}
	}

	return err
}

func (c *Conn) finish(r *Ctx, stream uint32, err error) {
	// Drop the stream before resolving: once RoundTrip returns it may hand the
	// Ctx back to a pool, and it can only do that when nothing here still
	// refers to it.
	if c.takeReq(stream) {
		atomic.AddInt32(&c.openStreams, -1)
	}

	c.deletePending(stream)

	r.markFinished()
	r.resolve(err)
}

func (c *Conn) readLoop() {
	defer func() { _ = c.Close() }()

	// A panic here would otherwise take the whole process down: this goroutine
	// parses whatever the server sends. Resolve the in-flight requests with the
	// failure instead, the way the write loop does.
	defer func() {
		err := recover()
		if err == nil {
			return
		}

		perr, ok := err.(error)
		if !ok {
			perr = fmt.Errorf("%v", err)
		}

		c.setLastErr(perr)

		for _, ctx := range c.takeAllReqs() {
			ctx.resolve(perr)
		}
	}()

	for {
		fr, err := c.readNext()
		if err != nil {
			c.setLastErr(err)
			break
		}

		// We advertise SETTINGS_ENABLE_PUSH of 0, so the server has no business
		// promising anything. Ignoring the frame would leave it holding a
		// stream we are never going to read.
		// https://httpwg.org/specs/rfc7540.html#rfc.section.6.5.2
		if fr.Type() == FramePushPromise {
			c.setLastErr(NewGoAwayError(ProtocolError, "server pushed with push disabled"))
			ReleaseFrameHeader(fr)

			break
		}

		// A stream-level WINDOW_UPDATE has to be applied whether or not a
		// request is still waiting on the stream, so it is handled before the
		// lookup in dispatch.
		if fr.Type() == FrameWindowUpdate {
			c.addWindow(fr.Stream(), int32(fr.Body().(*WindowUpdate).Increment()))
		}

		stop := c.dispatch(fr)

		ReleaseFrameHeader(fr)

		if stop {
			break
		}
	}
}

// dispatch hands a stream frame to the request waiting on it. It reports
// whether the read loop should stop.
func (c *Conn) dispatch(fr *FrameHeader) bool {
	r, ok := c.loadReq(fr.Stream())
	if !ok {
		return false
	}

	// A canceled or finished request has taken its Response back, so there is
	// nowhere to put this frame. Drop the stream and carry on.
	if !r.acquireFor(c, fr.Stream()) {
		c.dequeueReq(fr.Stream())

		return false
	}

	// Released on the way out even if readStream panics: leaving the Ctx locked
	// would wedge the RoundTrip that is waiting to take it back.
	defer r.release()

	endStream, err := c.readStream(fr, r)
	if err == nil {
		if endStream {
			c.finish(r, fr.Stream(), nil)
		}
	} else {
		c.finish(r, fr.Stream(), err)
	}

	if err != nil && errors.Is(err, FlowControlError) {
		return true
	}

	return c.state == connStateClosed && fr.Stream() == c.closeRef
}

func (c *Conn) writeRequest(ctx *Ctx) error {
	if !c.CanOpenStream() {
		return ErrNotAvailableStreams
	}

	// The request may have been canceled while it sat in the queue, in which
	// case its Request no longer belongs to us. Ownership is handed back
	// explicitly rather than deferred: sending the body takes it again, and the
	// lock is not reentrant.
	if !ctx.acquire() {
		return nil
	}

	released := false

	release := func() {
		if !released {
			released = true

			ctx.release()
		}
	}

	defer release()

	req := ctx.Request

	// Request.Body() reads a streamed body into memory, which is the one thing
	// a caller who reached for SetBodyStream asked us not to do, so the check
	// for a stream has to come first.
	bodyStream := req.IsBodyStream()
	hasBody := bodyStream || len(req.Body()) != 0

	// The server may have changed the header table size since the last request.
	// The encoder is the write loop's, so this is the only safe place to apply
	// it, and the encoder signals the change to the peer's decoder itself.
	if size := atomic.LoadUint32(&c.encTableSize); size != c.encTableSizeSeen {
		c.encTableSizeSeen = size
		c.enc.SetMaxTableSize(size)
	}

	enc := c.enc

	id := atomic.LoadUint32(&c.nextID)
	if id > maxStreamID {
		return ErrNoMoreStreamIDs
	}

	atomic.StoreUint32(&c.nextID, id+2)

	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()

	hf.SetBytes(StringAuthority, req.URI().Host())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringMethod, req.Header.Method())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringPath, req.URI().RequestURI())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringScheme, req.URI().Scheme())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(StringUserAgent, req.Header.UserAgent())
	enc.AppendHeaderField(h, hf, true)

	for k, v := range req.Header.All() {
		if bytes.EqualFold(k, StringUserAgent) {
			continue
		}

		// Lowercase hf's own copy of the name: k may point at fasthttp's
		// shared header name constants.
		hf.SetBytes(k, v)
		ToLower(hf.key)

		// Connection-specific fields are forbidden in HTTP/2 and a peer that
		// enforces the rule answers them with a stream error (RFC 7540 8.1.2.2).
		// fasthttp sets Transfer-Encoding: chunked on a body stream of unknown
		// length, for the benefit of its HTTP/1 writer; here END_STREAM ends
		// the body and there is nothing to say.
		if isConnectionSpecific(hf.key) {
			continue
		}

		enc.AppendHeaderField(h, hf, false)
	}

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	// store the ctx before sending the request
	ctx.conn.Store(c)
	atomic.StoreUint32(&ctx.streamID, id)
	c.queueReq(id, ctx)

	if hasBody {
		pb := &pendingBody{
			ctx:    ctx,
			window: c.streamWindow,
			size:   -1,
		}

		if bodyStream {
			pb.stream = req.BodyStream()
			pb.size = int64(req.Header.ContentLength())
			// A body declared as empty has nothing to read, and calling Read
			// on it would block on a reader that will never produce anything.
			pb.drained = pb.size == 0
		} else {
			pb.body = req.Body()
		}

		c.sendLck.Lock()
		c.pending[id] = pb
		c.sendLck.Unlock()
	}

	c.bwLck.Lock()

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}

	c.bwLck.Unlock()

	ReleaseHeaderField(hf)

	if err != nil {
		c.setLastErr(err)
		// if we had any error, remove it from the reqQueued.
		c.dequeueReq(id)
		c.deletePending(id)

		return err
	}

	atomic.AddInt32(&c.openStreams, 1)

	if hasBody {
		release()

		// The body goes out under flow control, so the tail of a large one may
		// have to wait for the server to open its window.
		return c.sendPending(id)
	}

	return nil
}

// applyInitialWindow adjusts every stream we are still sending on by the change
// in SETTINGS_INITIAL_WINDOW_SIZE.
func (c *Conn) applyInitialWindow(size int32) {
	c.sendLck.Lock()

	delta := size - c.streamWindow
	c.streamWindow = size

	for _, pb := range c.pending {
		pb.window += delta
	}

	c.sendLck.Unlock()

	c.signalWindow()
}

// addWindow grows a send window. Stream 0 is the connection window.
func (c *Conn) addWindow(streamID uint32, inc int32) {
	c.sendLck.Lock()

	if streamID == 0 {
		c.connWindow += inc
	} else if pb, ok := c.pending[streamID]; ok {
		pb.window += inc
	}

	c.sendLck.Unlock()

	c.signalWindow()
}

// signalWindow nudges the write loop. The channel holds one token: the loop
// only needs to know that something changed, not how many times.
func (c *Conn) signalWindow() {
	select {
	case c.winCh <- struct{}{}:
	default:
	}
}

func (c *Conn) deletePending(id uint32) {
	c.sendLck.Lock()
	pb := c.pending[id]
	delete(c.pending, id)
	c.sendLck.Unlock()

	if pb == nil || pb.stream == nil {
		return
	}

	// Taking the Ctx is what makes this safe: a request that has already been
	// handed back to its caller is theirs to close, and releasing it does.
	if !pb.ctx.acquireFor(c, id) {
		return
	}

	defer pb.ctx.release()

	c.closeBodyStream(pb)
}

// pendingIDs snapshots the streams with a body still to send.
func (c *Conn) pendingIDs() []uint32 {
	c.sendLck.Lock()
	defer c.sendLck.Unlock()

	if len(c.pending) == 0 {
		return nil
	}

	ids := make([]uint32, 0, len(c.pending))
	for id := range c.pending {
		ids = append(ids, id)
	}

	return ids
}

// flushPending writes whatever the send windows now allow. It runs on the write
// loop, which is the only place request bodies go out.
func (c *Conn) flushPending() error {
	for _, id := range c.pendingIDs() {
		if err := c.sendPending(id); err != nil {
			return err
		}
	}

	return nil
}

// sendPending writes as much of one blocked body as the connection and stream
// windows allow, and forgets the stream once the body is out.
//
// It loops because a streamed body only ever has one chunk buffered: an open
// window is worth more than that, and stopping after a chunk would trickle the
// body out one frame per WINDOW_UPDATE.
func (c *Conn) sendPending(id uint32) error {
	for {
		c.sendLck.Lock()

		pb, ok := c.pending[id]
		if !ok {
			c.sendLck.Unlock()
			return nil
		}

		// Nothing buffered and more to come: pull the next chunk. Read is the
		// caller's code and may block for as long as it likes, so it does not
		// run under the lock the read loop needs to hand window back.
		if len(pb.body) == 0 && pb.stream != nil && !pb.drained {
			c.sendLck.Unlock()

			if err := c.refillPending(pb); err != nil {
				// The body cannot be finished, and the peer is part way
				// through one it would otherwise wait for.
				c.deletePending(id)
				c.cancelStream(id, InternalError)

				return nil
			}

			continue
		}

		n := len(pb.body)
		if int(pb.window) < n {
			n = int(pb.window)
		}

		if int(c.connWindow) < n {
			n = int(c.connWindow)
		}

		if n < 0 {
			n = 0
		}

		pb.window -= int32(n)
		c.connWindow -= int32(n)

		body := pb.body[:n]
		pb.body = pb.body[n:]

		end := !pb.hasMore()
		if end {
			delete(c.pending, id)
		}

		c.sendLck.Unlock()

		if n == 0 && !end {
			return nil
		}

		// body points into the caller's Request, which stops being ours the
		// moment the request is canceled.
		if !pb.ctx.acquireFor(c, id) {
			c.deletePending(id)
			return nil
		}

		err := c.flushData(id, body, end)

		pb.ctx.release()

		if err != nil {
			return err
		}

		if end {
			c.closeBodyStream(pb)
			return nil
		}
	}
}

// flushData writes one run of DATA frames and flushes them. It is split out so
// that sendPending's loop does not hold bwLck across a Read on the caller's
// body stream.
func (c *Conn) flushData(id uint32, body []byte, end bool) error {
	c.bwLck.Lock()
	defer c.bwLck.Unlock()

	err := c.writeData(id, body, end)
	if err == nil {
		err = c.bw.Flush()
	}

	return err
}

// refillPending pulls the next chunk of a streamed request body into the
// body's own buffer.
func (c *Conn) refillPending(pb *pendingBody) error {
	// Read straight into the buffer the frames are cut from: going via a
	// scratch buffer would copy every byte of the body a second time.
	if cap(pb.buf) < int(defaultDataFrameSize) {
		pb.buf = make([]byte, defaultDataFrameSize)
	}

	buf := pb.buf[:defaultDataFrameSize]

	n, err := pb.stream.Read(buf)
	if n > 0 {
		pb.body = buf[:n]
		pb.read += int64(n)
	}

	switch {
	case errors.Is(err, io.EOF):
		pb.drained = true
	case err != nil:
		return err
	case n == 0:
		return errors.New("BUG: io.Reader returned 0, nil")
	}

	// A declared content length is the end of the body even if the reader has
	// not said so yet.
	if pb.size >= 0 && pb.read >= pb.size {
		pb.drained = true
	}

	return nil
}

// closeBodyStream closes a streamed request body once the connection is done
// with it. fasthttp closes the reader itself when its own writer sends the
// body, so whatever is behind it, a file or a pipe, stays open unless this
// does the same.
//
// The caller must hold the Ctx: the Request stops being ours the moment
// RoundTrip returns, and a caller that releases it closes the stream anyway.
func (c *Conn) closeBodyStream(pb *pendingBody) {
	if pb.stream == nil {
		return
	}

	pb.stream = nil

	_ = pb.ctx.Request.CloseBodyStream()
}

// writeData splits body into DATA frames no larger than the server is willing
// to receive. The caller holds bwLck.
func (c *Conn) writeData(id uint32, body []byte, end bool) (err error) {
	step := int(atomic.LoadUint32(&c.maxFrameSize))
	if step <= 0 || step > int(maxFrameSize) {
		step = int(defaultDataFrameSize)
	}

	fh := AcquireFrameHeader()
	defer ReleaseFrameHeader(fh)

	fh.SetStream(id)

	data := AcquireFrame(FrameData).(*Data)
	fh.SetBody(data)

	// The loop below writes nothing for an empty body, which would drop
	// END_STREAM and leave the request unfinished. A streamed body that turns
	// out to be empty, or one whose last read returned nothing, ends here.
	if len(body) == 0 {
		if !end {
			return nil
		}

		data.SetEndStream(true)
		data.SetPadding(false)
		data.SetData(nil)

		_, err = fh.WriteTo(c.bw)

		return err
	}

	for i := 0; err == nil && i < len(body); i += step {
		if i+step >= len(body) {
			step = len(body) - i
		}

		data.SetEndStream(end && i+step == len(body))
		data.SetPadding(false)
		data.SetData(body[i : step+i])

		_, err = fh.WriteTo(c.bw)
	}

	return err
}

func (c *Conn) readNext() (fr *FrameHeader, err error) {
loop:
	for err == nil {
		fr, err = ReadFrameFrom(c.br)
		if err != nil {
			// A frame of a type we do not know must be discarded rather than
			// treated as an error (RFC 7540 4.1). Its payload has already been
			// skipped by the reader, so there is nothing left to do but carry
			// on: extension frames such as ALTSVC, ORIGIN and PRIORITY_UPDATE
			// are the normal reason to see one.
			if errors.Is(err, ErrUnknownFrameType) {
				err = nil
				continue
			}

			break
		}

		if fr.Stream() != 0 {
			break
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if it has ack, just ignore
				c.handleSettings(st)
			}
		case FrameWindowUpdate:
			c.addWindow(0, int32(fr.Body().(*WindowUpdate).Increment()))
		case FramePing:
			ping := fr.Body().(*Ping)
			if !ping.IsAck() {
				c.handlePing(ping)
			} else {
				atomic.AddInt32(&c.unacks, -1)
			}
		case FrameGoAway:
			ga := fr.Body().(*GoAway)

			// Either way the server has stopped accepting new streams on this
			// connection, so the client must move to a fresh one.
			atomic.StoreUint32(&c.goAway, 1)

			if ga.stream == 0 {
				_ = c.c.Close()
				err = ga
			} else {
				// wait for the streams to complete
				c.closeRef = ga.stream
				c.state = connStateClosed
			}

			break loop
		}

		ReleaseFrameHeader(fr)
	}

	return fr, err
}

var ErrTimeout = errors.New("server is not replying to pings")

func (c *Conn) writePing() error {
	fr := AcquireFrameHeader()
	defer ReleaseFrameHeader(fr)

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	c.bwLck.Lock()
	defer c.bwLck.Unlock()

	_, err := fr.WriteTo(c.bw)
	if err == nil {
		err = c.bw.Flush()
		if err == nil {
			atomic.AddInt32(&c.unacks, 1)
		}
	}

	return err
}

func (c *Conn) handleSettings(st *Settings) {
	st.CopyTo(&c.serverS)

	atomic.StoreUint32(&c.maxStreams, c.serverS.MaxConcurrentStreams())
	atomic.StoreUint32(&c.maxFrameSize, c.serverS.MaxFrameSize())

	// The encoder belongs to the write loop, so the new table size is handed
	// over rather than applied here.
	atomic.StoreUint32(&c.encTableSize, st.HeaderTableSize())

	// A change to SETTINGS_INITIAL_WINDOW_SIZE applies to every stream that is
	// already open, as a delta on what it has left.
	// https://httpwg.org/specs/rfc7540.html#rfc.section.6.9.2
	if st.has(MaxWindowSize) {
		c.applyInitialWindow(int32(st.MaxWindowSize()))
	}

	// reply back
	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	c.writeOut(fr)
}

func (c *Conn) handlePing(ping *Ping) {
	// Reply back on a frame of our own: ping belongs to the frame header that
	// readNext releases right after this call, and reusing it would release the
	// same Ping into the pool twice.
	ack := AcquireFrame(FramePing).(*Ping)
	ack.SetAck(true)
	ack.SetData(ping.Data())

	fr := AcquireFrameHeader()
	fr.SetBody(ack)

	c.writeOut(fr)
}

// readStream applies one frame to the request waiting on the stream. It reports
// whether the response is complete.
func (c *Conn) readStream(fr *FrameHeader, ctx *Ctx) (endStream bool, err error) {
	res := ctx.Response

	switch fr.Type() {
	case FrameHeaders, FrameContinuation:
		// Only the whole block decodes. A field can be cut in half by the
		// frame boundary, which is what happens to any block that outgrows
		// SETTINGS_MAX_FRAME_SIZE, and decoding a fragment on its own leaves
		// the connection's decoder out of step with the peer's encoder for
		// good (RFC 7540 6.10).
		ctx.headerBlock = append(ctx.headerBlock, fr.Body().(FrameWithHeaders).Headers()...)

		if fr.Flags().Has(FlagEndStream) {
			ctx.blockEndStream = true
		}

		if !fr.Flags().Has(FlagEndHeaders) {
			return false, nil
		}

		err = c.readHeader(ctx.headerBlock, res)
		ctx.headerBlock = ctx.headerBlock[:0]

		if err != nil {
			return false, err
		}

		endStream, ctx.blockEndStream = ctx.blockEndStream, false

		return endStream, nil
	case FrameResetStream:
		// The server gave up on the stream. Without this the request would sit
		// there until MaxResponseTime, or forever if that check is disabled.
		return false, NewResetStreamError(
			fr.Body().(*RstStream).Code(), "stream reset by the server")
	case FrameData:
		c.currentWindow -= int32(fr.Len())
		currentWin := c.currentWindow

		data := fr.Body().(*Data)
		if data.Len() != 0 {
			res.AppendBody(data.Data())

			// let's send the window update
			c.updateWindow(fr.Stream(), fr.Len())
		}

		if currentWin < c.maxWindow/2 {
			nValue := c.maxWindow - currentWin

			c.currentWindow = c.maxWindow

			c.updateWindow(0, int(nValue))
		}
	}

	return fr.Flags().Has(FlagEndStream), nil
}

func (c *Conn) updateWindow(streamID uint32, size int) {
	fr := AcquireFrameHeader()

	fr.SetStream(streamID)

	wu := AcquireFrame(FrameWindowUpdate).(*WindowUpdate)
	wu.SetIncrement(size)

	fr.SetBody(wu)

	c.writeOut(fr)
}

func (c *Conn) readHeader(b []byte, res *fasthttp.Response) error {
	var err error
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	dec := c.dec

	var regularSeen bool

	for len(b) > 0 {
		b, err = dec.Next(hf, b)
		if err != nil {
			return err
		}

		// A response carries exactly one pseudo-header, :status, and it must
		// come before any regular field.
		// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.4
		if hf.IsPseudo() {
			if regularSeen {
				return errPseudoAfterRegular
			}

			if !bytes.Equal(hf.KeyBytes(), StringStatus) {
				return fmt.Errorf("invalid response pseudo-header %q", hf.KeyBytes())
			}

			n, err := parseUint(hf.ValueBytes())
			if err != nil || n < 100 || n > 999 {
				return errInvalidStatus
			}

			res.SetStatusCode(n)

			continue
		}

		regularSeen = true

		if hasUpperCase(hf.KeyBytes()) {
			return errUpperCaseHeader
		}

		if isConnectionSpecific(hf.KeyBytes()) {
			return errConnectionSpecific
		}

		if bytes.Equal(hf.KeyBytes(), StringContentLength) {
			n, err := parseUint(hf.ValueBytes())
			if err != nil {
				return errInvalidContentLength
			}

			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return nil
}

var (
	errPseudoAfterRegular   = errors.New("pseudo-header field after regular header field")
	errInvalidStatus        = errors.New("invalid :status pseudo-header")
	errUpperCaseHeader      = errors.New("header field name contains uppercase characters")
	errConnectionSpecific   = errors.New("connection-specific header field")
	errInvalidContentLength = errors.New("invalid content-length")
)
