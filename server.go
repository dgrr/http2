package http2

// Server ...
type Server struct {
}

func makeDefaultServer() *Server {
	return &Server{}
}

// serveConn ...
func (s *Server) serveConn(c net.Conn) (err error) {
	return
}
