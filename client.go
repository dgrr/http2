package http2

import (
	"container/list"
	"github.com/valyala/fasthttp"
	"sync"
)

type request struct {
	req *fasthttp.Request
	res *fasthttp.Response

	err chan error
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

type clientConn struct {
	c *Conn

	reqQueued sync.Map

	in chan *request
}

func (cl *Client) Do(req *fasthttp.Request, res *fasthttp.Response) (err error) {
	var cc *clientConn

getConn:
	cl.lck.Lock()
	e := cl.conns.Front()
	if e != nil {
		cc = e.Value.(*clientConn)
	} else {
		c, err := cl.d.Dial()
		if err != nil {
			cl.lck.Unlock()
			return err
		}

		cc = &clientConn{
			c:  c,
			in: make(chan *request, 128),
		}

		cl.conns.PushFront(cc)

		go cc.writeLoop()
		go cc.readLoop()
	}
	cl.lck.Unlock()

	if cc.c.Closed() {
		cl.conns.Remove(e)
		goto getConn
	}

	ch := make(chan error, 1)

	cc.in <- &request{
		req: req,
		res: res,
		err: ch,
	}

	select {
	case err = <-ch:
	}

	return err
}

func (cc *clientConn) writeLoop() {
	c := cc.c
	defer c.Close()

	for r := range cc.in {
		req := r.req

		uid, err := c.Write(req)
		if err != nil {
			break
		}

		cc.reqQueued.Store(uid, r)
	}
}

func (cc *clientConn) readLoop() {
	c := cc.c
	defer c.Close()

	for {
		fr, err := c.Next()
		if err != nil {
			break
		}

		// TODO: panic otherwise?
		if ri, ok := cc.reqQueued.Load(fr.Stream()); ok {
			r := ri.(*request)

			err := c.readStream(fr, r.res)
			if err == nil {
				if fr.Flags().Has(FlagEndStream) {
					cc.reqQueued.Delete(fr.Stream())

					close(r.err)
				}
			} else {
				cc.reqQueued.Delete(fr.Stream())

				r.err <- err
			}
		}

		ReleaseFrameHeader(fr)
	}

	close(cc.in)
}
