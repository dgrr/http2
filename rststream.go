package http2

import (
	"sync"
)

// RstStream ...
type RstStream struct {
	code uint32
}

var rstStreamPool = sync.Pool{
	New: func() interface{} {
		return &RstStream{}
	},
}

func AcquireRstStream() *RstStream {
	return rstStreamPool.Get().(*RstStream)
}

func ReleaseRstStream(rst *RstStream) {
	rst.Reset()
	rstStreamPool.Put(rst)
}

func (rst *RstStream) Reset() {
	rst.code = 0
}

// Error ...
func (rst *RstStream) Error() error {
	return Error(rst.code)
}

// Read ...
func (rst *RstStream) Read(b []byte) (n int, err error) {
	n = len(b)
	// TODO" check len ...
	rst.code = bytesToUint32(b)
	return
}

// ReadFrame ...
func (rst *RstStream) ReadFrame(fr *Frame) (err error) {
	_, err = rst.Read(fr.payload)
	return err
}
