package http2

import (
	"errors"
	"testing"
)

// TestPingCopyTo pins the direction: CopyTo copies the receiver into the
// argument, like every other CopyTo in the package, and it carries the payload.
func TestPingCopyTo(t *testing.T) {
	src := &Ping{}
	src.SetAck(true)
	src.SetData([]byte{1, 2, 3, 4, 5, 6, 7, 8})

	dst := &Ping{}
	src.CopyTo(dst)

	if !dst.IsAck() {
		t.Error("ack was not copied")
	}

	if got := dst.Data(); string(got) != string(src.Data()) {
		t.Errorf("data = %v, want %v", got, src.Data())
	}

	// The source must be untouched.
	if !src.IsAck() {
		t.Error("CopyTo modified the source")
	}
}

// TestWriteErrorWithoutStream covers the paths where there is no stream to
// report against. Each of these used to dereference a nil *Stream.
func TestWriteErrorWithoutStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "not a protocol error", err: errors.New("something else")},
		{name: "stream error", err: NewResetStreamError(ProtocolError, "no stream")},
		{name: "connection error", err: NewGoAwayError(ProtocolError, "no stream")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &serverConn{
				writer: make(chan *FrameHeader, 4),
				logger: discardLogger{},
			}

			sc.writeError(nil, tc.err)

			select {
			case fr := <-sc.writer:
				if _, ok := fr.Body().(*GoAway); !ok {
					t.Errorf("queued a %s, want a GOAWAY when there is no stream", fr.Type())
				}

				ReleaseFrameHeader(fr)
			default:
				t.Error("nothing was queued")
			}
		})
	}
}
