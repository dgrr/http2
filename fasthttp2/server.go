package fasthttp2

import (
	"net"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

// ConfigureServer configures the fasthttp's server to handle
// HTTP/2 connections. The HTTP/2 connection can be only
// established if the fasthttp server is using TLS.
//
// Future implementations may support HTTP/2 through plain TCP.
func ConfigureServer(s *fasthttp.Server) *Server {
	s2 := &Server{
		s: s,
	}

	s.NextProto(http2.H2TLSProto, s2.serveConn)

	return s2
}

type Server struct {
	s *fasthttp.Server
}

func (s *Server) serveConn(c net.Conn) error {
	return nil
}
