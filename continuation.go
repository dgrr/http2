package http2

import (
	"sync"
)

type Continuation struct {
	header []byte
}

var continuationPool = sync.Pool{
	New: func() interface{} {
		return &Continuation{}
	},
}

// AcquireContinuation ...
func AcquireContinuation() *Continuation {
	return continuationPool.Get().(*Continuation)
}

// ReleaseContinuation ...
func ReleaseContinuation(pp *Continuation) {
	pp.Reset()
	continuationPool.Put(pp)
}
