package http2

import (
	"sync"
)

type WindowUpdate struct {
	increment uint32
}

var windowUpdatePool = sync.Pool{
	New: func() interface{} {
		return &WindowUpdate{}
	},
}

// AcquireWindowUpdate ...
func AcquireWindowUpdate() *WindowUpdate {
	return windowUpdatePool.Get().(*WindowUpdate)
}

// ReleaseWindowUpdate ...
func ReleaseWindowUpdate(pp *WindowUpdate) {
	pp.Reset()
	windowUpdatePool.Put(pp)
}
