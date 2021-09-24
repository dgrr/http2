package http2

import (
	"container/list"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

const DefaultPingInterval = time.Second * 3

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
	Request  *fasthttp.Request
	Response *fasthttp.Response
	Err      chan error
}

type Client struct {
	d *Dialer

	// TODO: impl rtt
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

func (cl *Client) onConnectionDropped(c *Conn) {
	cl.lck.Lock()
	defer cl.lck.Unlock()

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

func (cl *Client) Do(req *fasthttp.Request, res *fasthttp.Response) (err error) {
	var c *Conn

	cl.lck.Lock()

	var next *list.Element

	for e := cl.conns.Front(); c == nil; e = next {
		if e != nil {
			c = e.Value.(*Conn)
		} else {
			c, e, err = cl.createConn()
			if err != nil {
				return err
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

	c.Write(&Ctx{
		Request:  req,
		Response: res,
		Err:      ch,
	})

	return <-ch
}
