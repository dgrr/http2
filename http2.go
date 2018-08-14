package fasthttp2

import (
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"sync"
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
	// TODO: Make a pool for prefaces?
	prefacePool = sync.Pool{
		New: func() interface{} {
			return make([]byte, prefaceLen)
		},
	}
)

// Stream states
const (
	StateIdle = iota
	StateOpen
	StateHalfClosed
	StateClosed
)

// copied from github.com/erikdubbelboer/fasthttp
type connTLSer interface {
	ConnectionState() tls.ConnectionState
}

func readPreface(br io.Reader) bool {
	b := prefacePool.Get().([]byte)
	defer prefacePool.Put(b)

	n, err := br.Read(b[:prefaceLen])
	if err == nil && n == prefaceLen {
		if bytes.Equal(b, http2Preface) {
			return true
		}
	}
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
