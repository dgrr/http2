package http2

import (
	"bytes"
	"crypto/tls"
	"io"

	"github.com/valyala/fasthttp"
)

// Byteorder must be big endian
// Values are unsigned unless otherwise indicated

const (
	// H2TLSProto is the string used in ALPN-TLS negotiation.
	H2TLSProto = "h2"
	// H2Clean is the string used in HTTP headers by the client to upgrade the connection.
	H2Clean = "h2c"
)

var (
	// http://httpwg.org/specs/rfc7540.html#ConnectionHeader
	http2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	prefaceLen   = len(http2Preface)
)

type State uint8

// Stream states
const (
	Idle State = iota
	Open
	HalfClosed
	Closed
)

// copied from github.com/valyala/fasthttp
type connTLSer interface {
	Handshake() error
	ConnectionState() tls.ConnectionState
}

// ReadPreface reads the connection initialisation preface.
func ReadPreface(br io.Reader) bool {
	b := make([]byte, prefaceLen)
	n, err := br.Read(b[:prefaceLen])
	if err == nil && n == prefaceLen {
		if bytes.Equal(b, http2Preface) {
			b = nil
			return true
		}
	}
	b = nil
	return false
}

// WritePreface writes HTTP/2 preface to the wr.
func WritePreface(wr io.Writer) error {
	_, err := wr.Write(http2Preface)
	return err
}

// UpgradeTLS checks if connection can be upgraded via TLS.
//
// returns true if the HTTP/2 using TLS-ALPN have been stablished.
func UpgradeTLS(c connTLSer) (ok bool) {
	// TODO: return error or false
	if err := c.Handshake(); err == nil {
		state := c.ConnectionState()
		// HTTP2 using TLS must be used with TLS1.2 or higher
		// (https://httpwg.org/specs/rfc7540.html#TLSUsage)
		if state.Version >= tls.VersionTLS12 {
			ok = state.NegotiatedProtocol == H2TLSProto &&
				state.NegotiatedProtocolIsMutual
		}
	}
	return
}

// UpgradeHTTP checks if connection can be upgraded via HTTP.
//
// returns true if the HTTP/2 using Upgrade header field have been stablished.
func UpgradeHTTP(ctx *fasthttp.RequestCtx) bool {
	// TODO:
	return false
}
