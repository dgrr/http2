package http2

import (
	"container/list"
	"sync"

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

getConn:
	cl.lck.Lock()

	e := cl.conns.Front()
	if e != nil {
		c = e.Value.(*Conn)
	} else {
		var err error

		c, err = cl.d.Dial()
		if err != nil {
			cl.lck.Unlock()
			return err
		}

		e = cl.conns.PushFront(c)
	}

	if c.Closed() {
		cl.conns.Remove(e)
		cl.lck.Unlock()

		goto getConn
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
