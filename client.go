package http2

import (
	"container/list"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

const DefaultPingInterval = time.Second * 5

// ClientOpts defines the client options for the HTTP/2 connection.
type ClientOpts struct {
	// PingInterval defines the interval in which the client will ping the server.
	//
	// An interval of 0 will make the library to use DefaultPingInterval. Because ping intervals can't be disabled.
	PingInterval time.Duration

	// OnRTT is assigned to every client after creation, and the handler
	// will be called after every RTT measurement (after receiving a PONG mesage).
	OnRTT func(time.Duration)
}

// Ctx represents a context for a stream. Every stream is related to a context.
type Ctx struct {
	// Request ...
	Request *fasthttp.Request
	// Response ...
	Response *fasthttp.Response
	// Err ...
	Err chan error
}

// Client ...
type Client struct {
	d *Dialer

	onRTT func(time.Duration)

	lck   sync.Mutex
	conns list.List
}

func createClient(d *Dialer, opts ClientOpts) *Client {
	cl := &Client{
		d:     d,
		onRTT: opts.OnRTT,
	}

	return cl
}

func (cl *Client) Do(req *fasthttp.Request, res *fasthttp.Response) (err error) {
	var c *Conn

	cl.lck.Lock()

	var next *list.Element

	for e := cl.conns.Front(); c == nil; e = next {
		if e != nil {
			c = e.Value.(*Conn)
		} else {
			var err error

			if c, err = cl.d.Dial(ConnOpts{
				PingInterval: cl.d.PingInterval,
			}); err != nil {
				cl.lck.Unlock()
				return err
			}

			e = cl.conns.PushFront(c)
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

	c.Write(&Ctx{
		Request:  req,
		Response: res,
		Err:      ch,
	})

	select {
	case err = <-ch:
	}

	return err
}
