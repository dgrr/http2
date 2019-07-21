package http2

import (
	"strconv"
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

	b bytebufferpool.ByteBuffer
}

// AcquireResponse ...
func AcquireResponse() *Response {
	return resPool.Get().(*Response)
}

func (res *Response) Reset() {
	res.Header.Reset()
	res.b.Reset()
}

func (res *Response) Write(b []byte) (int, error) {
	n, _ := res.b.Write(b)
	res.Header.contentLength += n
	return n, nil
}

func (res *Response) Body() []byte {
	return res.b.Bytes()
}

// ResponseHeader ...
type ResponseHeader struct {
	hs            []*HeaderField
	hp            *HPACK
	raw           []byte
	contentLength int
	statusCode    int
}

func (h *ResponseHeader) Reset() {
	h.hs = h.hs[:0]
	h.raw = h.raw[:0]
	h.statusCode = 0
}

// Get ...
func (h *ResponseHeader) Get(key string) (hf *HeaderField) {
	for i := range h.hs {
		if b2s(h.hs[i].key) == key {
			hf = h.hs[i]
			break
		}
	}

	return
}

// GetString ...
func (h *ResponseHeader) GetBytes(key []byte) *HeaderField {
	return h.Get(b2s(key))
}

func (h *ResponseHeader) SetStatusCode(code int) {
	h.statusCode = code
}

func (h *ResponseHeader) Add(k, v string) {
	hf := AcquireHeaderField()
	hf.Set(k, v)
	h.hs = append(h.hs, hf)
}

func (h *ResponseHeader) parse() {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	if h.statusCode <= 0 {
		h.statusCode = 200
	}
	hf.SetKey(":status")
	hf.value = strconv.AppendInt(hf.value[:0], int64(h.statusCode), 10)
	h.raw = h.hp.AppendHeader(hf, h.raw[:0])

	if h.contentLength > 0 {
		hf.SetKey("content-length")
		hf.value = strconv.AppendInt(hf.value[:0], int64(h.contentLength), 10)
		h.raw = h.hp.AppendHeader(hf, h.raw)
	}
}
