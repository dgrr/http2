package http2

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
	window int
	state  StreamState
	data   interface{}
}

func NewStream(id uint32, win int, data interface{}) *Stream {
	return &Stream{
		id:     id,
		window: win,
		state:  StreamStateIdle,
		data:   data,
	}
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

func (s *Stream) Window() int {
	return s.window
}

func (s *Stream) SetWindow(win int) {
	s.window = win
}

func (s *Stream) IncrWindow(win int) {
	s.window += win
}

func (s *Stream) Data() interface{} {
	return s.data
}
