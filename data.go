package http2

import (
	"sync"
)

// Data defines a FrameData
type Data struct {
	noCopy noCopy
	ended  bool
	pad    bool
	b      []byte // data bytes
}

var dataPool = sync.Pool{
	New: func() interface{} {
		return &Data{}
	},
}

// AcquireData ...
func AcquireData() (data *Data) {
	data = dataPool.Get().(*Data)
	return
}

// ReleaseData ...
func ReleaseData(data *Data) {
	data.Reset()
	dataPool.Put(data)
}

// Reset ...
func (data *Data) Reset() {
	data.ended = false
	data.b = data.b[:0]
}

// CopyTo copies data to d.
func (data *Data) CopyTo(d *Data) {
	d.pad = data.pad
	d.ended = data.ended
	d.b = append(d.b[:0], data.b...)
}

// SetEndStream ...
func (data *Data) SetEndStream(value bool) {
	data.ended = value
}

// Data returns the byte slice of the data readed/to be sended.
func (data *Data) Data() []byte {
	return data.b
}

// SetData resets data byte slice and sets b.
func (data *Data) SetData(b []byte) {
	data.b = append(data.b[:0], b...)
}

// Padding returns true if the data will be/was padded.
func (data *Data) Padding() bool {
	return data.pad
}

// SetPadding sets padding to the data if true. In false the data won't be padded.
func (data *Data) SetPadding(value bool) {
	data.pad = value
}

// Append appends b to data
func (data *Data) Append(b []byte) {
	data.b = append(data.b, b...)
}

func (data *Data) Len() uint32 {
	return uint32(len(data.b))
}

// Write writes b to data
func (data *Data) Write(b []byte) (int, error) {
	n := len(b)
	data.Append(b)
	return n, nil
}

// ReadFrame reads data from fr.
func (data *Data) ReadFrame(fr *Frame) error {
	payload := cutPadding(fr)
	data.ended = fr.Has(FlagEndStream)
	data.b = append(data.b[:0], payload...)
	return nil
}

// WriteFrame writes the data to the frame payload setting FlagPadded.
//
// This function only resets the frame payload.
func (data *Data) WriteFrame(fr *Frame) error {
	// TODO: generate padding and set to the frame payload
	// fr.SetPayload(padding)
	if data.pad {
		fr.Add(FlagPadded)
		// TODO: Write padding flag
	}
	_, err := fr.Write(data.b)
	return err
}
