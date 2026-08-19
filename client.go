package http2

import (
	"container/list"
	"errors"
	"io"
	"sync"
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
}

// resolve will resolve the context, meaning that provided an error,
func (ctx *Ctx) resolve(err error) {
	select {
	case ctx.Err <- err:
	default:
	}
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

func (cl *Client) RoundTrip(_ *fasthttp.HostClient, req *fasthttp.Request, res *fasthttp.Response) (retry bool, err error) {
	var c *Conn

	cl.lck.Lock()

	if cl.closed {
		cl.lck.Unlock()
		return false, ErrClientClosed
	}

	var next *list.Element

	for e := cl.conns.Front(); c == nil; e = next {
		if e != nil {
			c = e.Value.(*Conn)
		} else {
			c, e, err = cl.createConn()
			if err != nil {
				// Unlock explicitly: an early return used to leave the mutex
				// held, wedging every later request on this client.
				cl.lck.Unlock()
				return false, err
			}
		}

		// if we can't open a stream, then move on to the next one.
		if !c.CanOpenStream() {
			c = nil
			next = e.Next()
		}

		// if the connection has been closed, then just remove the connection.
		if c != nil && c.Closed() {
			next = e.Next()
			cl.conns.Remove(e)
			c = nil
		}
	}

	cl.lck.Unlock()

	ch := make(chan error, 1)

	var cancelTimer *time.Timer

	ctx := &Ctx{
		Request:  req,
		Response: res,
		Err:      ch,
	}

	if cl.opts.MaxResponseTime > 0 {
		cancelTimer = time.AfterFunc(cl.opts.MaxResponseTime, func() {
			// resolve rather than a bare send: the stream may have been
			// answered already, in which case the buffer is full and a send
			// would block this timer goroutine forever.
			ctx.resolve(ErrRequestCanceled)
			c.cancel(ctx)
		})
	}

	c.Write(ctx)

	err = <-ch

	if cancelTimer != nil {
		cancelTimer.Stop()
	}

	// ch is deliberately left open. The connection can still resolve this Ctx
	// after we return (a late frame on the stream, or the cancel timer racing
	// Stop), and resolving a closed channel panics.

	return false, err
}
