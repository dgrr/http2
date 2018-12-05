package http2

import (
	"sync"
)

// Headers defines a FrameHeaders
type Headers struct {
	noCopy     noCopy
	pad        bool
	stream     uint32
	weight     byte // TODO: byte or uint8?
	hpack      *HPACK
	endStream  bool
	endHeaders bool
}

var headersPool = sync.Pool{
	New: func() interface{} {
		return &Headers{}
	},
}

// AcquireHeaders ...
func AcquireHeaders() *Headers {
	h := headersPool.Get().(*Headers)
	h.hpack = AcquireHPACK()
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
	ReleaseHPACK(h.hpack)
	h.hpack = nil
	h.endStream = false
	h.endHeaders = false
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
	h.stream = stream
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
func (h *Headers) ReadFrame(fr *Frame) (err error) {
	payload := cutPadding(fr)
	if fr.Has(FlagPriority) {
		h.stream = bytesToUint32(payload) & (1<<31 - 1)
		h.weight = payload[4]
		payload = payload[5:]
	}
	h.endStream = fr.Has(FlagEndStream)
	h.endHeaders = fr.Has(FlagEndHeaders)
	_, err = h.hpack.Read(payload)
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
	fr.payload, err = h.hpack.Write(fr.payload)
	// TODO: Write padding
	return err
}
