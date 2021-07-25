package http2

import (
	"container/list"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

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

func createClient(d *Dialer) *Client {
	cl := &Client{
		d: d,
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

			if c, err = cl.d.Dial(); err != nil {
				cl.lck.Unlock()
				return err
			}

			e = cl.conns.PushFront(c)
		}

		if !c.CanOpenStream() {
			c = nil
			next = e.Next()
		}

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
