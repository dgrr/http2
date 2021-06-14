package http2

import (
	"sync"
)

// StreamState ...
type StreamState int8

const (
	StreamStateIdle StreamState = iota
	StreamStateReserved
	StreamStateOpen
	StreamStateHalfClosed
	StreamStateClosed
)

func (ss StreamState) String() string {
	switch ss {
	case StreamStateIdle:
		return "Idle"
	case StreamStateReserved:
		return "Reserved"
	case StreamStateOpen:
		return "Open"
	case StreamStateHalfClosed:
		return "HalfClosed"
	case StreamStateClosed:
		return "Closed"
	}

	return "IDK"
}

type Stream struct {
	id     uint32
	window int32
	state  StreamState
	data   interface{}
}

var streamPool = sync.Pool{
	New: func() interface{} {
		return &Stream{}
	},
}

func NewStream(id uint32, win int32) *Stream {
	strm := streamPool.Get().(*Stream)
	strm.id = id
	strm.window = win
	strm.state = StreamStateIdle
	strm.data = nil

	return strm
}

func (s *Stream) ID() uint32 {
	return s.id
}

func (s *Stream) SetID(id uint32) {
	s.id = id
}

func (s *Stream) State() StreamState {
	return s.state
}

func (s *Stream) SetState(state StreamState) {
	s.state = state
}

func (s *Stream) Window() int32 {
	return s.window
}

func (s *Stream) SetWindow(win int32) {
	s.window = win
}

func (s *Stream) IncrWindow(win int32) {
	s.window += win
}

func (s *Stream) Data() interface{} {
	return s.data
}

func (s *Stream) SetData(data interface{}) {
	s.data = data
}
