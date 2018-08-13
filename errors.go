package fasthttp2

import (
	"errors"
)

const (
	// Error codes (http://httpwg.org/specs/rfc7540.html#ErrorCodes)
	//
	// Errors must be uint32 because of FrameReset
	NoError              uint32 = 0x0
	ProtocolError        uint32 = 0x1
	InternalError        uint32 = 0x2
	FlowControlError     uint32 = 0x3
	SettingsTimeoutError uint32 = 0x4
	StreamClosedError    uint32 = 0x5
	FrameSizeError       uint32 = 0x6
	RefusedStreamError   uint32 = 0x7
	CancelError          uint32 = 0x8
	CompressionError     uint32 = 0x9
	ConnectionError      uint32 = 0xa
	EnhanceYourCalm      uint32 = 0xb
	InadequateSecurity   uint32 = 0xc
	HTTP11Required       uint32 = 0xd
)

type errHTTP2 struct {
	err         error
	frameToSend byte
}

func (e errHTTP2) Error() string {
	return e.err.Error()
}

func Error(code uint32) error {
	if code >= 0x0 && code <= 0x13 {
		return errHTTP2{
			err:         errParser[code],
			frameToSend: FrameResetStream,
		}
	}
	return nil
}

var (
	errParser = []error{
		NoError:              errors.New("No errors"),
		ProtocolError:        errors.New("Protocol error"),
		InternalError:        errors.New("Internal error"),
		FlowControlError:     errors.New("Flow control error"),
		SettingsTimeoutError: errors.New("Settings timeout"),
		StreamClosedError:    errors.New("Stream have been closed"),
		FrameSizeError:       errors.New("Frame size error"),
		RefusedStreamError:   errors.New("Refused Stream"),
		CancelError:          errors.New("Canceled"),
		CompressionError:     errors.New("Compression error"),
		ConnectionError:      errors.New("Connection error"),
		EnhanceYourCalm:      errors.New("Enhance your calm"),
		InadequateSecurity:   errors.New("Inadequate security"),
		HTTP11Required:       errors.New("HTTP/1.1 required"),
	}
	// This error codes must be used with FrameGoAway
	errUnknowFrameType = errors.New("error bad frame type")
	errZeroPayload     = errors.New("frame Payload len = 0")
	errBadPreface      = errors.New("bad preface size")
	errFrameMismatch   = errors.New("frame type mismatch from called function")
	errNilWriter       = errors.New("Writer cannot be nil")
	errNilReader       = errors.New("Reader cannot be nil")
	errUnknown         = errors.New("Unknown error")
	errBitOverflow     = errors.New("Bit overflow")
)
