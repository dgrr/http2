package fasthttp2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
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

// upgradeTLS returns true if TLS upgrading have been successful
func upgradeTLS(c net.Conn) bool {
	cc, isTLS := c.(connTLSer)
	if isTLS {
		state := cc.ConnectionState()
		if state.NegotiatedProtocol != h2TLSProto || state.Version < tls.VersionTLS12 {
			// TODO: Follow security recommendations?
			return false
		}
		// HTTP2 using TLS must be used with TLS1.2 or higher
		// (https://httpwg.org/specs/rfc7540.html#TLSUsage)
		return readPreface(c)
	}
	return false
}

// Only TLS Upgrade
/*
func upgradeHTTP(ctx *RequestCtx) bool {
	if ctx.Request.Header.ConnectionUpgrade() &&
		b2s(ctx.Request.Header.Peek("Upgrade")) == h2Clean {
		// HTTP2 connection stablished via HTTP
		ctx.SetStatusCode(StatusSwitchingProtocols)
		ctx.Response.Header.Add("Connection", "Upgrade")
		ctx.Response.Header.Add("Upgrade", h2Clean)
		// TODO: Add support for http2-settings header value
		return true
	}
	// The server connection preface consists of a potentially empty SETTINGS frame
	// (Section 6.5) that MUST be the first frame the server sends in the HTTP/2 connection.
	// (https://httpwg.org/specs/rfc7540.html#ConnectionHeader)
	// TODO: Write settings frame
	// TODO: receive preface
	// TODO: Read settings and store it in Request.st
	return false
}
*/
