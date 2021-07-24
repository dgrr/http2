package http2

import (
	"github.com/golang-design/lockfree"
)

// Client ...
type Client struct {
	c lockfree.Stack
}
