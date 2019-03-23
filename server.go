package http2

import (
	"net"

	"github.com/valyala/fasthttp"
)

// Server ...
type Server struct {
	Handler fasthttp.RequestHandler
}

func makeDefaultServer() *Server {
	return &Server{}
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) (err error) {
	return
}
