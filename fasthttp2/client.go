package fasthttp2

import (
	"errors"

	"github.com/valyala/fasthttp"
)

var (
	ErrServerSupport = errors.New("server doesn't support HTTP/2")
)

type Client struct {
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	return nil
}
