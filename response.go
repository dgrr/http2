package http2

import (
	"sync"

	"github.com/valyala/bytebufferpool"
)

var resPool = sync.Pool{
	New: func() interface{} {
		return new(Response)
	},
}

type Response struct {
	h ResponseHeaders
	b *bytebufferpool.ByteBuffer
}

// AcquireResponse ...
func AcquireResponse() *Response {
	return resPool.Get().(*Response)
}

// ResponseHeaders ...
type ResponseHeaders struct {
	h []*HeaderField
}

// Get ...
func (h *ResponseHeaders) Get(key []byte) *HeaderField {
	return h.GetString(b2s(key))
}

// GetString ...
func (h *ResponseHeaders) GetString(key string) (hf *HeaderField) {
	for i := range h.h {
		if b2s(h.h[i].key) == key {
			hf = h.h[i]
			break
		}
	}

	return
}
