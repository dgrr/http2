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
	Header ResponseHeader

	b *bytebufferpool.ByteBuffer
}

// AcquireResponse ...
func AcquireResponse() *Response {
	return resPool.Get().(*Response)
}

func (res *Response) Reset() {
	res.Header.Reset()
	res.b.Reset()
}

// ResponseHeader ...
type ResponseHeader struct {
	hs []*HeaderField
	hp *HPACK
}

func (h *ResponseHeader) Reset() {
	h.hs = h.hs[:0]
}

// Get ...
func (h *ResponseHeader) Get(key []byte) *HeaderField {
	return h.GetString(b2s(key))
}

// GetString ...
func (h *ResponseHeader) GetString(key string) (hf *HeaderField) {
	for i := range h.hs {
		if b2s(h.hs[i].key) == key {
			hf = h.hs[i]
			break
		}
	}

	return
}
