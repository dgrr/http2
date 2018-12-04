package http2

import (
	"io"
	"sync"
)

// Data defines a FrameData
type Data struct {
	noCopy noCopy
	ended  bool
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
	payload := fr.payload
	if fr.Has(FlagPadded) {
		padding := uint32(payload[0])
		payload = payload[1 : fr.length-padding]
	}
	data.b = append(data.b[:0], payload...)
	return nil
}

// WriteTo writes data to the wr.
//
// wr can be Frame. Cause frame is compatible with io.Writer.
func (data *Data) WriteTo(wr io.Writer) (nn int64, err error) {
	var n int
	// TODO: Generate padding ...
	n, err = wr.Write(data.b)
	nn += int64(n)
	return
}
