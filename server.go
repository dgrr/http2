package http2

import (
	"net"
)

type RequestHandler func(*Ctx)

// Server ...
type Server struct {
	Handler RequestHandler
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) (err error) {
	// Read the settings frame.
	// Ctx-per-stream
	for {

	}
	return
}
