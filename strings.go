package http2

var (
	strPath          = []byte(":path")
	strStatus        = []byte(":status")
	strAuthority     = []byte(":authority")
	strScheme        = []byte(":scheme")
	strMethod        = []byte(":method")
	strServer        = []byte("server")
	strContentLength = []byte("content-length")
	strContentType   = []byte("content-type")
	strUserAgent     = []byte("user-agent")
	strGET           = []byte("GET")
	strHEAD          = []byte("HEAD")
	strPOST          = []byte("POST")
	strHTTP2         = []byte("HTTP/2")
)

func toLower(b []byte) []byte {
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
