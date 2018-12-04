package http2

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
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

func readPreface(br io.Reader) bool {
	// TODO: make a preface pool?
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

// writePreface writes HTTP/2 preface to the writer
func writePreface(wr io.Writer) error {
	_, err := wr.Write(http2Preface)
	return err
}

// Upgrade upgrades the HTTP(S) connection.
//
// returns a boolean value indicating if the upgrade was successfully
func Upgrade(c net.Conn) (ok bool) {
	var tlsConn connTLSer
	if tlsConn, ok = c.(connTLSer); ok {
		ok = upgradeTLS(tlsConn)
	} else {
		ok = upgradeHTTP(c)
	}
	return
}

// upgradeTLS returns true if TLS upgrading have been successful
// TODO: Public?
func upgradeTLS(c connTLSer) (ok bool) {
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

// TODO: public?
func upgradeHTTP(c net.Conn) bool {
	// TODO:
	return false
}
