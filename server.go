package http2

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"
)

type ServerAdaptor interface {
	OnNewStream(net.Conn, *Stream)
	OnFrame(*Stream, *FrameHeader, *HPACK) error
	OnRequestFinished(*Stream, *HPACK, chan<- *FrameHeader)
	OnStreamEnd(*Stream)
}

type Server struct {
	ln net.Listener

	Adaptor ServerAdaptor
}

type serverConn struct {
	adpr ServerAdaptor
	c    net.Conn

	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
	dec *HPACK

	lastID uint32

	clientWindow       int32
	clientStreamWindow int32
	maxWindow          int32
	currentWindow      int32

	writer chan *FrameHeader
	reader chan *FrameHeader

	st      Settings
	clientS Settings
}

func (s *Server) ServeConn(c net.Conn) error {
	defer c.Close()

	if !ReadPreface(c) {
		return errors.New("wrong preface")
	}

	sc := &serverConn{
		adpr:   s.Adaptor,
		c:      c,
		br:     bufio.NewReader(c),
		bw:     bufio.NewWriterSize(c, 1<<14*10),
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		lastID: 0,
		writer: make(chan *FrameHeader, 128),
		reader: make(chan *FrameHeader, 128),
	}

	sc.maxWindow = 1 << 22
	sc.currentWindow = sc.maxWindow

	sc.st.Reset()
	sc.st.SetMaxWindowSize(uint32(sc.maxWindow))
	sc.st.SetMaxConcurrentStreams(1024)

	if err := Handshake(false, sc.bw, &sc.st, sc.maxWindow); err != nil {
		return err
	}

	go sc.handleStreams()
	go sc.writeLoop()

	defer func() {
		close(sc.writer)
		close(sc.reader)
	}()

	var (
		fr  *FrameHeader
		err error
	)

	for err == nil {
		fr, err = ReadFrameFrom(sc.br)
		if err != nil {
			break
		}

		if fr.Stream() != 0 {
			if fr.Stream() < atomic.LoadUint32(&sc.lastID) || fr.Stream()&1 == 0 {
				writeReset(fr.Stream(), ProtocolError, sc.writer)
			} else {
				sc.reader <- fr
			}

			continue
		}

		switch fr.Type() {
		case FrameSettings:
			st := fr.Body().(*Settings)
			if !st.IsAck() { // if has ack, just ignore
				sc.handleSettings(st)
			}
		case FrameWindowUpdate:
			win := int32(fr.Body().(*WindowUpdate).Increment())

			if !atomic.CompareAndSwapInt32(&sc.clientWindow, 0, win) {
				atomic.AddInt32(&sc.clientWindow, win)
			}
		case FramePing:
			// sc.handlePing(fr.Body().(*Ping))
		case FrameGoAway:
			ga := fr.Body().(*GoAway)
			if ga.Code() == NoError {
				err = io.EOF
			} else {
				err = fmt.Errorf("goaway: %s: %s", ga.Code(), ga.Data())
			}
		}

		ReleaseFrameHeader(fr)
	}

	if err == io.EOF {
		err = nil
	}

	return err
}

func (sc *serverConn) handleStreams() {
	var strms = make(map[uint32]*Stream)

	for {
		select {
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			strm, ok := strms[fr.Stream()]
			if !ok { // then create it
				if len(strms) > int(sc.st.maxStreams) {
					writeReset(fr.Stream(), RefusedStreamError, sc.writer)
					continue
				}

				strm = NewStream(fr.Stream(), sc.clientStreamWindow)
				strms[fr.Stream()] = strm

				atomic.StoreUint32(&sc.lastID, strm.ID())

				sc.adpr.OnNewStream(sc.c, strm)
			}

			if err := sc.adpr.OnFrame(strm, fr, sc.dec); err != nil {
				writeError(strm, err, sc.writer)
			}

			handleState(fr, strm)

			switch strm.State() {
			case StreamStateHalfClosed:
				sc.adpr.OnRequestFinished(strm, sc.enc, sc.writer)
				fallthrough
			case StreamStateClosed:
				sc.adpr.OnStreamEnd(strm)
				delete(strms, strm.ID())
				streamPool.Put(strm)
			}
		}
	}
}

func (sc *serverConn) writeLoop() {
	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

	buffered := 0

loop:
	for {
		select {
		case fr, ok := <-sc.writer:
			if !ok {
				break loop
			}

			_, err := fr.WriteTo(sc.bw)
			if err == nil && (len(sc.writer) == 0 || buffered > 10) {
				err = sc.bw.Flush()
				buffered = 0
			} else if err == nil {
				buffered++
			}

			ReleaseFrameHeader(fr)

			if err != nil {
				// TODO: handle errors
				return
			}
		case <-ticker.C:
			// sc.sendPing()
		}
	}
}

func (sc *serverConn) handleSettings(st *Settings) {
	st.CopyTo(&sc.clientS)

	atomic.StoreInt32(&sc.clientStreamWindow, int32(sc.clientS.MaxWindowSize()))

	fr := AcquireFrameHeader()

	stRes := AcquireFrame(FrameSettings).(*Settings)
	stRes.SetAck(true)

	fr.SetBody(stRes)

	sc.writer <- fr
}
