package http2

import (
	"sync"
)

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
	hpack      *HPACK
	rawHeaders []byte // this field is used to store uncompleted headers.
}

var headersPool = sync.Pool{
	New: func() interface{} {
		return &Headers{
			hpack: AcquireHPACK(),
		}
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
	ReleaseHPACK(h.hpack)
	h.hpack = nil
	headersPool.Put(h)
}

// Reset ...
func (h *Headers) Reset() {
	h.pad = false
	h.stream = 0
	h.weight = 0
	h.hpack.Reset()
	h.endStream = false
	h.endHeaders = false
	h.rawHeaders = h.rawHeaders[:0]
}

// CopyTo copies h fields to h2.
func (h *Headers) CopyTo(h2 *Headers) {
	h2.pad = h.pad
	h2.stream = h.stream
	h2.weight = h.weight
	if h2.hpack != nil {
		ReleaseHPACK(h2.hpack)
	}
	h2.hpack = h.hpack
	h2.endStream = h.endStream
	h2.endHeaders = h.endHeaders
	h2.rawHeaders = append(h2.rawHeaders[:0], h.rawHeaders...)
}

// Add adds a name and value to the HPACK header.
func (h *Headers) Add(name, value string) {
	h.hpack.Add(name, value)
}

// AddBytes adds a name and value to the HPACK header.
func (h *Headers) AddBytes(name, value []byte) {
	h.hpack.AddBytes(name, value)
}

// AddBytesK ...
func (h *Headers) AddBytesK(name []byte, value string) {
	h.hpack.AddBytesK(name, value)
}

// AddBytesV ...
func (h *Headers) AddBytesV(name string, value []byte) {
	h.hpack.AddBytesV(name, value)
}

// RawHeaders ...
func (h *Headers) RawHeaders() []byte {
	return h.rawHeaders
}

// SetHeaders ...
func (h *Headers) SetRawHeaders(b []byte) {
	h.rawHeaders = append(h.rawHeaders[:0], b...)
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

// HPACK returns the HPACK of Headers.
//
// Do not release this field by your own.
func (h *Headers) HPACK() *HPACK {
	return h.hpack
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
		h.rawHeaders = append(h.rawHeaders, fr.payload...)
	}
	return
}

// WriteFrame writes h into fr,
//
// This function only resets the payload
func (h *Headers) WriteFrame(fr *Frame) (err error) {
	if h.endStream {
		fr.Add(FlagEndStream)
	}
	if h.endHeaders {
		fr.Add(FlagEndHeaders)
	}
	fr._type = FrameHeaders

	fr.payload = fr.payload[:0]
	if h.pad {
		// TODO: Write padding len
		fr.Add(FlagPadded)
	}
	if h.stream > 0 && h.weight > 0 {
		fr.Add(FlagPriority)
		// TODO: Write stream and weight
	}
	// TODO: Writing header directly is an error?
	fr.payload, err = h.hpack.Write(fr.payload)
	// TODO: Write padding
	return err
}
