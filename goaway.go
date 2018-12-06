package http2

import (
	"sync"
)

type GoAway struct {
	stream uint32
	code   uint32
	data   []byte // additional data
}

var goawayPool = sync.Pool{
	New: func() interface{} {
		return &GoAway{}
	},
}

// AcquireGoAway ...
func AcquireGoAway() *GoAway {
	return goawayPool.Get().(*GoAway)
}

// ReleaseGoAway ...
func ReleaseGoAway(pp *GoAway) {
	pp.Reset()
	goawayPool.Put(pp)
}
