package http2

import (
	"bytes"
	"errors"
	"strconv"
)

var (
	StringPath          = []byte(":path")
	StringStatus        = []byte(":status")
	StringAuthority     = []byte(":authority")
	StringScheme        = []byte(":scheme")
	StringMethod        = []byte(":method")
	StringServer        = []byte("server")
	StringContentLength = []byte("content-length")
	StringContentType   = []byte("content-type")
	StringUserAgent     = []byte("user-agent")
	StringGzip          = []byte("gzip")
	StringGET           = []byte("GET")
	StringHEAD          = []byte("HEAD")
	StringPOST          = []byte("POST")
	StringHTTP2         = []byte("HTTP/2")

	// Connection-specific header fields that are forbidden in HTTP/2.
	// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2.2
	StringConnection       = []byte("connection")
	StringKeepAlive        = []byte("keep-alive")
	StringProxyConnection  = []byte("proxy-connection")
	StringTransferEncoding = []byte("transfer-encoding")
	StringUpgrade          = []byte("upgrade")
	StringTE               = []byte("te")
	StringTrailers         = []byte("trailers")
)

// hasUpperCase reports whether b contains an uppercase ASCII letter.
// HTTP/2 header field names must be lowercase.
// https://httpwg.org/specs/rfc7540.html#rfc.section.8.1.2
func hasUpperCase(b []byte) bool {
	for _, c := range b {
		if c >= 'A' && c <= 'Z' {
			return true
		}
	}

	return false
}

// parseUint parses a non-negative base-10 integer from b. It errors on empty
// input or any non-digit byte.
func parseUint(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, errInvalidUint
	}

	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, errInvalidUint
		}

		n = n*10 + int(c-'0')
	}

	return n, nil
}

var errInvalidUint = errors.New("invalid unsigned integer")

// isConnectionSpecific reports whether the (lowercase) header name is a
// connection-specific field forbidden in HTTP/2.
func isConnectionSpecific(k []byte) bool {
	switch {
	case bytes.Equal(k, StringConnection),
		bytes.Equal(k, StringKeepAlive),
		bytes.Equal(k, StringProxyConnection),
		bytes.Equal(k, StringTransferEncoding),
		bytes.Equal(k, StringUpgrade):
		return true
	}

	return false
}

func ToLower(b []byte) []byte {
	for i := range b {
		b[i] |= 32
	}

	return b
}

const (
	// H2TLSProto is the string used in ALPN-TLS negotiation.
	H2TLSProto = "h2"
	// H2Clean is the string used in HTTP headers by the client to upgrade the connection.
	H2Clean = "h2c"
)

// statusCodes holds the decimal form of every three-digit status code, so
// writing a response header does not format one.
var statusCodes = func() [1000][]byte {
	var codes [1000][]byte

	for i := 100; i < 1000; i++ {
		codes[i] = []byte(strconv.Itoa(i))
	}

	return codes
}()

// statusBytes returns the decimal form of a status code. Anything outside the
// three digits HTTP/2 allows is reported as 500: the alternative is writing a
// :status the peer has to reject.
func statusBytes(code int) []byte {
	if code < 100 || code > 999 {
		code = 500
	}

	return statusCodes[code]
}
