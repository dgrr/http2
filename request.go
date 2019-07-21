package http2

import (
	"bytes"
	"errors"
	"sync"

	"github.com/valyala/bytebufferpool"
)

var reqPool = sync.Pool{
	New: func() interface{} {
		return new(Request)
	},
}

type Request struct {
	Header RequestHeader

	b        bytebufferpool.ByteBuffer
	lastType uint8 // last type of frame
}

// AcquireRequest ...
func AcquireRequest() *Request {
	return reqPool.Get().(*Request)
}

func (req *Request) Reset() {
	req.Header.Reset()
	req.b.Reset()
}

func (req *Request) Body() []byte {
	return req.b.Bytes()
}

var (
	errCannotHandle      = errors.New("cannot handle this frame type")
	errLastTypeDontMatch = errors.New("last type doesn't match any")
)

func (req *Request) Read(fr *Frame) error {
	data := AcquireData()

	err := data.ReadFrame(fr)
	if err == nil {
		req.b.Write(data.b)
	}

	ReleaseData(data)

	return err
}

// RequestHeader ...
type RequestHeader struct {
	path      []byte
	method    []byte
	userAgent []byte

	h      []*HeaderField
	parsed bool

	hp  *HPACK
	raw []byte
}

func (h *RequestHeader) IsGet() bool {
	return bytes.Equal(h.method, strGET)
}

func (h *RequestHeader) IsHead() bool {
	return bytes.Equal(h.method, strHEAD)
}

func (h *RequestHeader) IsPost() bool {
	return bytes.Equal(h.method, strPOST)
}

func (h *RequestHeader) Path() []byte {
	return h.path
}

func (h *RequestHeader) Method() []byte {
	return h.method
}

func (h *RequestHeader) UserAgent() []byte {
	return h.userAgent
}

func (h *RequestHeader) Reset() {
	// TODO: free resources
	h.h = h.h[:0]
	h.raw = h.raw[:0]
}

func (h *RequestHeader) Read(fr *Frame) {
	hfr := AcquireHeaders()
	err := hfr.ReadFrame(fr)
	if err == nil {
		//if fr.Has(FlagEndHeaders) {
		h.parsed = fr.Has(FlagEndHeaders)
		h.parse(hfr.rawHeaders)
		//}
	}
	ReleaseHeaders(hfr)
}

func (h *RequestHeader) parse(b []byte) (err error) {
	hp := h.hp
	hf := AcquireHeaderField()

	for len(b) > 0 {
		b, err = hp.Next(hf, b)
		if err != nil {
			break
		}
		if len(hf.key) > 0 {
			switch hf.key[0] | 0x20 {
			case 'u':
				if equalsFold(hf.key, strUserAgent) {
					h.userAgent = append(h.userAgent[:0], hf.value...)
					continue
				}
			case ':':
				hf.key = append(hf.key[:0], hf.key[1:]...)
				if len(hf.key) == 0 {
					continue
				}

				switch hf.key[0] {
				case 'p':
					if equalsFold(hf.key, strPath) {
						h.path = append(h.path[:0], hf.value...)
						continue
					}
				case 'm':
					if equalsFold(hf.key, strMethod) {
						h.method = append(h.method[:0], hf.value...)
						continue
					}
				}
			}
			h.h = append(h.h, hf)
			hf = AcquireHeaderField()
		}
	}

	ReleaseHeaderField(hf)

	return err
}

func (h *RequestHeader) Write(b []byte) (int, error) {
	h.raw = append(h.raw, b...)
	return len(b), nil
}

func (h *RequestHeader) Peek(key string) []byte {
	hf := h.Get(key)
	if hf != nil {
		return hf.value
	}
	return nil
}

// Get ...
func (h *RequestHeader) Get(key string) (hf *HeaderField) {
	for i := range h.h {
		if b2s(h.h[i].key) == key {
			hf = h.h[i]
			break
		}
	}

	return
}

// GetBytes ...
func (h *RequestHeader) GetBytes(key []byte) *HeaderField {
	return h.Get(b2s(key))
}
