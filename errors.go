package http2

import (
	"errors"
	"fmt"
)

type ErrorCode uint32

const (
	// Error codes (http://httpwg.org/specs/rfc7540.html#ErrorCodes)
	//
	// Errors must be uint32 because of FrameReset
	NoError              ErrorCode = 0x0
	ProtocolError        ErrorCode = 0x1
	InternalError        ErrorCode = 0x2
	FlowControlError     ErrorCode = 0x3
	SettingsTimeoutError ErrorCode = 0x4
	StreamClosedError    ErrorCode = 0x5
	FrameSizeError       ErrorCode = 0x6
	RefusedStreamError   ErrorCode = 0x7
	CancelError          ErrorCode = 0x8
	CompressionError     ErrorCode = 0x9
	ConnectionError      ErrorCode = 0xa
	EnhanceYourCalm      ErrorCode = 0xb
	InadequateSecurity   ErrorCode = 0xc
	HTTP11Required       ErrorCode = 0xd
)

type Error struct {
	err string
	msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.err, e.msg)
}

// NewError returns error of uint32 declared error codes.
func NewError(code ErrorCode, msg string) error {
	return &Error{err: errParser[code], msg: msg}
}

var (
	errParser = []string{
		NoError:              "No errors",
		ProtocolError:        "Protocol error",
		InternalError:        "Internal error",
		FlowControlError:     "Flow control error",
		SettingsTimeoutError: "Settings timeout",
		StreamClosedError:    "Stream have been closed",
		FrameSizeError:       "FrameHeader size error",
		RefusedStreamError:   "Refused Stream",
		CancelError:          "Canceled",
		CompressionError:     "Compression error",
		ConnectionError:      "Connection error",
		EnhanceYourCalm:      "Enhance your calm",
		InadequateSecurity:   "Inadequate security",
		HTTP11Required:       "HTTP/1.1 required",
	}

	// This error codes must be used with FrameGoAway
	ErrUnknowFrameType = errors.New("unknown frame type")
	ErrZeroPayload     = errors.New("frame Payload len = 0")
	ErrMissingBytes    = errors.New("missing payload bytes. Need more")
	ErrTooManyBytes    = errors.New("too many bytes")
	ErrBadPreface      = errors.New("wrong preface")
	ErrFrameMismatch   = errors.New("frame type mismatch from called function")
	ErrNilWriter       = errors.New("Writer cannot be nil")
	ErrNilReader       = errors.New("Reader cannot be nil")
	ErrUnknown         = errors.New("Unknown error")
	ErrBitOverflow     = errors.New("Bit overflow")
	ErrPayloadExceeds  = errors.New("FrameHeader payload exceeds the negotiated maximum size")
)
