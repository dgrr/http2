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

// Error returns error of uint32 declared error codes.
func Error(code uint32) error {
	if code >= 0x0 && code <= 0xd {
		return errParser[code]
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
	ErrUnknowFrameType = errors.New("error unknown frame type")
	ErrZeroPayload     = errors.New("frame Payload len = 0")
	ErrBadPreface      = errors.New("bad preface size")
	ErrFrameMismatch   = errors.New("frame type mismatch from called function")
	ErrNilWriter       = errors.New("Writer cannot be nil")
	ErrNilReader       = errors.New("Reader cannot be nil")
	ErrUnknown         = errors.New("Unknown error")
	ErrBitOverflow     = errors.New("Bit overflow")
	ErrPayloadExceeds  = errors.New("Frame payload exceeds the negotiated maximum size")
)
