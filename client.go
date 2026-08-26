package http2

import (
	"container/list"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

const (
	DefaultPingInterval    = time.Second * 3
	DefaultMaxResponseTime = time.Minute
)

// ClientOpts defines the client options for the HTTP/2 connection.
type ClientOpts struct {
	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of 0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled.
	PingInterval time.Duration

	// MaxResponseTime defines a timeout to wait for the server's response.
	// If the server doesn't reply within MaxResponseTime the stream will be canceled.
	//
	// If MaxResponseTime is 0, DefaultMaxResponseTime will be used.
	// If MaxResponseTime is <0, the max response timeout check will be disabled.
	MaxResponseTime time.Duration

	// OnRTT is assigned to every client after creation, and the handler
	// will be called after every RTT measurement (after receiving a PONG message).
	OnRTT func(time.Duration)
}

func (opts *ClientOpts) sanitize() {
	if opts.MaxResponseTime == 0 {
		opts.MaxResponseTime = DefaultMaxResponseTime
	}

	if opts.PingInterval <= 0 {
		opts.PingInterval = DefaultPingInterval
	}
}

// Ctx represents a context for a stream. Every stream is related to a context.
type Ctx struct {
	Request  *fasthttp.Request
	Response *fasthttp.Response
	Err      chan error

	streamID uint32

	// Request and Response belong to whoever called RoundTrip, and that caller
	// is free to release them the instant RoundTrip returns. The connection
	// reads Request on the write loop and fills Response on the read loop, so
	// both have to take ownership before touching either, and RoundTrip has to
	// take it back before it returns. Without this a canceled request is a use
	// after free: the write loop reads a Request the caller has already put
	// back in a pool.
	lck  sync.Mutex
	done bool

	// conn is the connection the request went out on, for the cancel timer.
	conn atomic.Pointer[Conn]

	// resLck guards everything to do with delivering the answer. It is separate
	// from lck because the loops resolve a Ctx while they hold lck.
	resLck   sync.Mutex
	resolved bool
	finished bool

	// timer is the cancel timer, kept with the Ctx so that reusing one does not
	// mean allocating a timer and a closure per request.
	timer *time.Timer
	armed bool

	// headerBlock accumulates the fragments of a header block that is still
	// arriving. A block is one HEADERS frame plus the CONTINUATION frames that
	// follow it, and only their concatenation is a valid HPACK block.
	headerBlock []byte

	// blockEndStream records END_STREAM seen on a HEADERS frame whose block is
	// not complete yet. END_STREAM ends the message body, not the header
	// block, so the response is only finished once END_HEADERS arrives
	// (RFC 7540 6.2).
	blockEndStream bool
}

// acquire takes ownership of the Ctx for the connection. It reports false once
// RoundTrip has handed Request and Response back to its caller, in which case
// the caller of acquire must not touch either and must not unlock.
func (ctx *Ctx) acquire() bool {
	ctx.lck.Lock()

	if ctx.done {
		ctx.lck.Unlock()
		return false
	}

	return true
}

// acquireFor is acquire with a check that the Ctx is still the one that belongs
// to this stream on this connection. A finished Ctx goes back to a pool, so a
// pointer held past that point can end up pointing at somebody else's request.
func (ctx *Ctx) acquireFor(c *Conn, id uint32) bool {
	ctx.lck.Lock()

	if ctx.done || ctx.conn.Load() != c || atomic.LoadUint32(&ctx.streamID) != id {
		ctx.lck.Unlock()
		return false
	}

	return true
}

func (ctx *Ctx) release() {
	ctx.lck.Unlock()
}

// takeBack blocks until the connection is not using the Ctx and stops it from
// using it again. After it returns, Request and Response are the caller's.
func (ctx *Ctx) takeBack() {
	ctx.lck.Lock()
	ctx.done = true
	ctx.lck.Unlock()

	ctx.resLck.Lock()
	ctx.resolved = true
	ctx.resLck.Unlock()
}

// markFinished records that the connection has dropped the stream from its
// tables, which is what makes the Ctx safe to reuse.
func (ctx *Ctx) markFinished() {
	ctx.resLck.Lock()
	ctx.finished = true
	ctx.resLck.Unlock()
}

// resolve will resolve the context, meaning that provided an error,
func (ctx *Ctx) resolve(err error) {
	ctx.resLck.Lock()

	if !ctx.resolved {
		select {
		case ctx.Err <- err:
		default:
		}
	}

	ctx.resLck.Unlock()
}

// fireTimeout runs when MaxResponseTime is up.
func (ctx *Ctx) fireTimeout() {
	// resolve rather than a bare send: the stream may have been answered
	// already, in which case the buffer is full and a send would block this
	// timer goroutine forever.
	ctx.resolve(ErrRequestCanceled)

	if c := ctx.conn.Load(); c != nil {
		c.cancel(ctx)
	}
}

// reusable reports whether the Ctx can go back in the pool: the connection has
// finished with it and the cancel timer is not about to run.
func (ctx *Ctx) reusable() bool {
	stopped := true

	if ctx.armed {
		ctx.armed = false
		stopped = ctx.timer.Stop()
	}

	ctx.resLck.Lock()
	defer ctx.resLck.Unlock()

	return stopped && ctx.finished
}

var clientCtxPool = sync.Pool{
	New: func() interface{} {
		ctx := &Ctx{
			Err: make(chan error, 1),
		}

		// time.AfterFunc costs a timer and a closure. Building it once per
		// pooled Ctx keeps that off the per-request path.
		ctx.timer = time.AfterFunc(timerDisarmed, ctx.fireTimeout)
		ctx.timer.Stop()

		return ctx
	},
}

func acquireCtx(req *fasthttp.Request, res *fasthttp.Response) *Ctx {
	ctx := clientCtxPool.Get().(*Ctx)

	// Nothing else refers to a Ctx that came out of the pool, so these are
	// plain writes. A resolve that landed after the last caller stopped reading
	// would still be sitting in the buffer.
	select {
	case <-ctx.Err:
	default:
	}

	ctx.Request = req
	ctx.Response = res
	ctx.streamID = 0
	ctx.done = false
	ctx.resolved = false
	ctx.finished = false
	ctx.armed = false
	ctx.headerBlock = ctx.headerBlock[:0]
	ctx.blockEndStream = false

	ctx.conn.Store(nil)

	return ctx
}

func releaseCtx(ctx *Ctx) {
	ctx.Request = nil
	ctx.Response = nil

	ctx.conn.Store(nil)

	clientCtxPool.Put(ctx)
}

type Client struct {
	d *Dialer

	opts ClientOpts

	lck    sync.Mutex
	conns  list.List
	closed bool
}

// ErrClientClosed is returned by RoundTrip after the Client has been closed.
var ErrClientClosed = errors.New("client is closed")

// Close closes every connection the client holds and stops it from opening new
// ones. Each connection runs a read and a write goroutine, so a client that is
// dropped without being closed leaks both those goroutines and the connection's
// buffers for the life of the process.
//
// A closed Client cannot be reused.
func (cl *Client) Close() error {
	cl.lck.Lock()

	if cl.closed {
		cl.lck.Unlock()
		return nil
	}

	cl.closed = true

	conns := make([]*Conn, 0, cl.conns.Len())
	for e := cl.conns.Front(); e != nil; e = e.Next() {
		conns = append(conns, e.Value.(*Conn))
	}

	cl.conns.Init()

	// Closing a connection calls back into onConnectionDropped, so the lock has
	// to be released first.
	cl.lck.Unlock()

	var err error

	for _, c := range conns {
		if cerr := c.Close(); cerr != nil && !errors.Is(cerr, io.EOF) && err == nil {
			err = cerr
		}
	}

	return err
}

func createClient(d *Dialer, opts ClientOpts) *Client {
	opts.sanitize()

	cl := &Client{
		d:    d,
		opts: opts,
	}

	return cl
}

func (cl *Client) onConnectionDropped(c *Conn) {
	cl.lck.Lock()
	defer cl.lck.Unlock()

	// Do not dial a replacement for a connection we closed on purpose.
	if cl.closed {
		return
	}

	for e := cl.conns.Front(); e != nil; e = e.Next() {
		if e.Value.(*Conn) == c {
			cl.conns.Remove(e)

			_, _, _ = cl.createConn()

			break
		}
	}
}

func (cl *Client) createConn() (*Conn, *list.Element, error) {
	c, err := cl.d.Dial(ConnOpts{
		PingInterval: cl.d.PingInterval,
		OnDisconnect: cl.onConnectionDropped,
	})
	if err != nil {
		return nil, nil, err
	}

	return c, cl.conns.PushFront(c), nil
}

var ErrRequestCanceled = errors.New("request timed out")

// pickConn returns a connection with room for another stream, dialing one if
// none of the connections the client holds has any.
func (cl *Client) pickConn() (*Conn, error) {
	cl.lck.Lock()
	defer cl.lck.Unlock()

	if cl.closed {
		return nil, ErrClientClosed
	}

	// Walk what we have, dropping the connections that have closed, and dial at
	// most one replacement. The old form of this loop went back to the start of
	// the list after every failed candidate, so a server that advertises no
	// concurrent streams kept the client dialing for ever.
	var next *list.Element

	for e := cl.conns.Front(); e != nil; e = next {
		next = e.Next()

		conn := e.Value.(*Conn)

		if conn.Closed() {
			cl.conns.Remove(e)
			continue
		}

		if conn.CanOpenStream() {
			return conn, nil
		}
	}

	c, _, err := cl.createConn()

	return c, err
}

// roundTripAttempts bounds how many connections one request will try before
// giving up. Each attempt costs nothing but a pick, because a retry only
// happens when the request never reached the wire.
const roundTripAttempts = 4

func (cl *Client) RoundTrip(_ *fasthttp.HostClient, req *fasthttp.Request, res *fasthttp.Response) (retry bool, err error) {
	for attempt := 0; ; attempt++ {
		err = cl.roundTripOnce(req, res)
		if err == nil || !retryable(err) {
			return false, err
		}

		// Nothing went out, so this can go on another connection whatever the
		// method is. Picking again skips the connection that just turned it
		// away: it either has no streams left or has closed, and pickConn
		// checks both.
		if attempt == roundTripAttempts-1 {
			// A request that never reached the server can be sent again.
			// fasthttp decides whether to, under its own retry policy:
			// idempotent methods only, up to MaxIdemponentCallAttempts.
			return true, err
		}
	}
}

func (cl *Client) roundTripOnce(req *fasthttp.Request, res *fasthttp.Response) error {
	c, err := cl.pickConn()
	if err != nil {
		return err
	}

	ctx := acquireCtx(req, res)

	if cl.opts.MaxResponseTime > 0 {
		ctx.armed = true
		ctx.timer.Reset(cl.opts.MaxResponseTime)
	}

	c.Write(ctx)

	err = <-ctx.Err

	// Both loops may still be part way through this Ctx, and the caller is free
	// to reuse Request and Response as soon as we return.
	reuse := ctx.reusable()

	ctx.takeBack()

	// Err is deliberately never closed. The connection can still resolve this
	// Ctx after we return (a late frame on the stream, or the cancel timer
	// racing Stop), and resolving a closed channel panics. A Ctx only goes back
	// in the pool when the connection has let go of it and the timer is not
	// about to run, so nothing can reach the next request through it.
	if reuse {
		releaseCtx(ctx)
	}

	return err
}

// retryable reports whether the request definitely did not reach the server.
func retryable(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, ErrConnectionClosed) ||
		errors.Is(err, ErrNotAvailableStreams) ||
		errors.Is(err, ErrNoMoreStreamIDs)
}
