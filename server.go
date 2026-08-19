package http2

import (
	"bufio"
	"errors"
	"net"
	"time"

	"github.com/valyala/fasthttp"
)

// DefaultMaxHeaderListSize is the header list size a server accepts when
// ServerConfig.MaxHeaderListSize is left at zero.
const DefaultMaxHeaderListSize = 1 << 20

// ServerConfig ...
type ServerConfig struct {
	// PingInterval is the interval at which the server will send a
	// ping message to a client.
	//
	// To disable pings set the PingInterval to a negative value.
	PingInterval time.Duration

	// ...
	MaxConcurrentStreams int

	// MaxHeaderListSize bounds the uncompressed size of a request header list,
	// summed across the HEADERS frame and every CONTINUATION frame that
	// completes it. Each field counts as name + value + 32 bytes, per RFC 7540
	// 6.5.2. Exceeding it is a connection error.
	//
	// Without a limit a header block that never sets END_HEADERS grows the
	// server's memory for as long as the client keeps sending, which is the
	// CONTINUATION flood.
	//
	// Zero means DefaultMaxHeaderListSize. A negative value disables the check.
	MaxHeaderListSize int

	// Debug is a flag that will allow the library to print debugging information.
	Debug bool
}

func (sc *ServerConfig) defaults() {
	if sc.PingInterval == 0 {
		sc.PingInterval = time.Second * 10
	}

	if sc.MaxConcurrentStreams <= 0 {
		sc.MaxConcurrentStreams = 1024
	}

	if sc.MaxHeaderListSize == 0 {
		sc.MaxHeaderListSize = DefaultMaxHeaderListSize
	}
}

// Server defines an HTTP/2 entity that can handle HTTP/2 connections.
type Server struct {
	s *fasthttp.Server

	cnf ServerConfig
}

// ServeConn starts serving a net.Conn as HTTP/2.
//
// This function will fail if the connection does not support the HTTP/2 protocol.
func (s *Server) ServeConn(c net.Conn) error {
	defer func() { _ = c.Close() }()

	if !ReadPreface(c) {
		return errors.New("wrong preface")
	}

	sc := &serverConn{
		c:              c,
		h:              s.s.Handler,
		br:             bufio.NewReader(c),
		bw:             bufio.NewWriterSize(c, 1<<14*10),
		lastID:         0,
		writer:         make(chan *FrameHeader, 128),
		reader:         make(chan *FrameHeader, 128),
		maxRequestTime: s.s.ReadTimeout,
		maxIdleTime:    s.s.IdleTimeout,
		pingInterval:   s.cnf.PingInterval,
		maxHeaderList:  s.cnf.MaxHeaderListSize,
		logger:         s.s.Logger,
		debug:          s.cnf.Debug,
	}

	if sc.logger == nil {
		sc.logger = logger
	}

	sc.enc.Reset()
	sc.dec.Reset()

	sc.maxWindow = 1 << 22
	sc.currentWindow = sc.maxWindow

	sc.st.Reset()
	sc.st.SetMaxWindowSize(uint32(sc.maxWindow))
	sc.st.SetMaxConcurrentStreams(uint32(s.cnf.MaxConcurrentStreams))

	// Advertise the limit so a well-behaved client stops before we have to cut
	// the connection.
	if sc.maxHeaderList > 0 {
		sc.st.SetMaxHeaderListSize(uint32(sc.maxHeaderList))
	}

	if err := sc.Handshake(); err != nil {
		return err
	}

	return sc.Serve()
}
