package http2

import (
	"sync"

	"github.com/valyala/bytebufferpool"
)

var reqPool = sync.Pool{
	New: func() interface{} {
		return new(Request)
	},
}

type Request struct {
	h RequestHeaders
	b *bytebufferpool.ByteBuffer
}

// AcquireRequest ...
func AcquireRequest() *Request {
	return reqPool.Get().(*Request)
}

func (req *Request) Reset() {
	req.h.Reset()
	req.b.Reset()
}

// RequestHeaders ...
type RequestHeaders struct {
	h []*HeaderField
}

func (h *RequestHeaders) Reset() {
	// TODO: free resources
	h.h = h.h[:0]
}

// Get ...
func (h *RequestHeaders) Get(key []byte) *HeaderField {
	return h.GetString(b2s(key))
}

// GetString ...
func (h *RequestHeaders) GetString(key string) (hf *HeaderField) {
	for i := range h.h {
		if b2s(h.h[i].key) == key {
			hf = h.h[i]
			break
		}
	}

	return
}
