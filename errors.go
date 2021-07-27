package http2

import (
	"fmt"
	"strconv"
)

// ErrorCode defines the HTTP/2 error codes:
//
// Error codes are defined here http://httpwg.org/specs/rfc7540.html#ErrorCodes
//
// Errors must be uint32 because of FrameReset
type ErrorCode uint32

const (
	NoError              ErrorCode = 0x0
	ProtocolError                  = 0x1
	InternalError                  = 0x2
	FlowControlError               = 0x3
	SettingsTimeoutError           = 0x4
	StreamClosedError              = 0x5
	FrameSizeError                 = 0x6
	RefusedStreamError             = 0x7
	CancelError                    = 0x8
	CompressionError               = 0x9
	ConnectionError                = 0xa
	EnhanceYourCalm                = 0xb
	InadequateSecurity             = 0xc
	HTTP11Required                 = 0xd
)

func (e ErrorCode) Error() string {
	if int(e) < len(errParser) {
		return errParser[e]
	}

	return strconv.Itoa(int(e))
}

type Error struct {
	code  ErrorCode
	debug string
}

func (e Error) Code() ErrorCode {
	return e.code
}

func NewError(e ErrorCode, debug string) Error {
	return Error{
		code:  e,
		debug: debug,
	}
}

func (e Error) Error() string {
	return fmt.Sprintf("%s: %s", e.code, e.debug)
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
	ErrUnknowFrameType = NewError(
		ProtocolError, "unknown frame type")
	ErrMissingBytes = NewError(
		ProtocolError, "missing payload bytes. Need more")
	ErrPayloadExceeds = NewError(
		ProtocolError, "FrameHeader payload exceeds the negotiated maximum size")
)
