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
	h2TLSProto = "h2"
	h2Clean    = "h2c"
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

// copied from github.com/erikdubbelboer/fasthttp
type connTLSer interface {
	ConnectionState() tls.ConnectionState
}

func readPreface(br io.Reader) bool {
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

func Upgrade(c net.Conn) bool {
	_, ok := c.(connTLSer)
	if ok {
		ok = upgradeTLS(c)
	} else {
		ok = upgradeHTTP(c)
	}
	if ok {
		ok = readPreface(c)
	}
	return ok
}

// upgradeTLS returns true if TLS upgrading have been successful
func upgradeTLS(c net.Conn) bool {
	state := c.(connTLSer).ConnectionState()
	if state.NegotiatedProtocol != h2TLSProto || state.Version < tls.VersionTLS12 {
		// TODO: Follow security recommendations?
		return false
	}
	// HTTP2 using TLS must be used with TLS1.2 or higher
	// (https://httpwg.org/specs/rfc7540.html#TLSUsage)
	return true
}

func upgradeHTTP(c net.Conn) bool {
	// TODO:
	return false
}
