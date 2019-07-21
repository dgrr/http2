package http2

import (
	"sync"
)

const FrameHeaders uint8 = 0x1

// Headers defines a FrameHeaders
//
// https://tools.ietf.org/html/rfc7540#section-6.2
type Headers struct {
	noCopy     noCopy
	pad        bool
	stream     uint32
	weight     byte // TODO: byte or uint8?
	endStream  bool
	endHeaders bool
	parsed     bool
	rawHeaders []byte // this field is used to store uncompleted headers.
}

var headersPool = sync.Pool{
	New: func() interface{} {
		return &Headers{}
	},
}

// AcquireHeaders ...
func AcquireHeaders() *Headers {
	h := headersPool.Get().(*Headers)
	return h
}

// ReleaaseHeaders ...
func ReleaseHeaders(h *Headers) {
	h.Reset()
	headersPool.Put(h)
}

// Reset ...
func (h *Headers) Reset() {
	h.pad = false
	h.stream = 0
	h.weight = 0
	h.endStream = false
	h.endHeaders = false
	h.parsed = false
	h.rawHeaders = h.rawHeaders[:0]
}

// CopyTo copies h fields to h2.
func (h *Headers) CopyTo(h2 *Headers) {
	h2.pad = h.pad
	h2.parsed = h.parsed
	h2.stream = h.stream
	h2.weight = h.weight
	h2.endStream = h.endStream
	h2.endHeaders = h.endHeaders
	h2.rawHeaders = append(h2.rawHeaders[:0], h.rawHeaders...)
}

// RawHeaders ...
func (h *Headers) Headers() []byte {
	return h.rawHeaders
}

// SetHeaders ...
func (h *Headers) SetHeaders(b []byte) {
	h.rawHeaders = append(h.rawHeaders[:0], b...)
}

// AppendRawHeaders appends b to the raw headers.
func (h *Headers) AppendRawHeaders(b []byte) {
	h.rawHeaders = append(h.rawHeaders, b...)
}

// EndStream ...
func (h *Headers) EndStream() bool {
	return h.endStream
}

// SetEndHeaders ...
func (h *Headers) SetEndStream(value bool) {
	h.endStream = value
}

// EndHeaders ...
func (h *Headers) EndHeaders() bool {
	return h.endHeaders
}

// SetEndHeaders ...
func (h *Headers) SetEndHeaders(value bool) {
	h.endHeaders = value
}

// Stream ...
func (h *Headers) Stream() uint32 {
	return h.stream
}

// SetStream ...
func (h *Headers) SetStream(stream uint32) {
	h.stream = stream & (1<<31 - 1)
}

// Weight ...
func (h *Headers) Weight() byte {
	return h.weight
}

// SetWeight ...
func (h *Headers) SetWeight(w byte) {
	h.weight = w
}

// Padding ...
func (h *Headers) Padding() bool {
	return h.pad
}

// SetPadding sets padding value ...
func (h *Headers) SetPadding(value bool) {
	h.pad = value
}

// ReadFrame reads header data from fr.
//
// This function appends over rawHeaders .....
func (h *Headers) ReadFrame(fr *Frame) (err error) {
	payload := cutPadding(fr)
	if fr.Has(FlagPriority) {
		if len(fr.payload) < 5 { // 4 (stream) + 1 (weight) = 5
			err = ErrMissingBytes
		} else {
			h.stream = bytesToUint32(payload) & (1<<31 - 1)
			h.weight = payload[4]
			payload = payload[5:]
		}
	}
	if err == nil {
		h.endStream = fr.Has(FlagEndStream)
		h.endHeaders = fr.Has(FlagEndHeaders)
		h.rawHeaders = append(h.rawHeaders, payload...)
	}

	return
}
