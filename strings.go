package http2

var (
	strUserAgent = []byte("user-agent")
	strPath      = []byte("path")
	strMethod    = []byte("method")
	strGET       = []byte("GET")
	strHEAD      = []byte("HEAD")
	strPOST      = []byte("POST")
)

const (
	// H2TLSProto is the string used in ALPN-TLS negotiation.
	H2TLSProto = "h2"
	// H2Clean is the string used in HTTP headers by the client to upgrade the connection.
	H2Clean = "h2c"
)
