package http2

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

type ServerAdaptor interface {
	OnNewStream(*Stream)
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

	nextID uint32
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
	if !ReadPreface(c) {
		return errors.New("wrong preface")
	}

	sc := &serverConn{
		adpr:   s.Adaptor,
		c:      c,
		br:     bufio.NewReader(c),
		bw:     bufio.NewWriter(c),
		enc:    AcquireHPACK(),
		dec:    AcquireHPACK(),
		nextID: 2,
		lastID: 0,
		writer: make(chan *FrameHeader, 128),
		reader: make(chan *FrameHeader, 128),
	}
	defer sc.c.Close()

	sc.maxWindow = 1 << 20
	sc.currentWindow = sc.maxWindow

	sc.st.SetMaxWindowSize(1 << 16)
	if err := Handshake(sc.bw, &sc.st, sc.maxWindow); err != nil {
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
			sc.reader <- fr
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
			err = fmt.Errorf("goaway: %s", fr.Body().(*GoAway).Code())
		}

		ReleaseFrameHeader(fr)
	}

	return err
}

func (sc *serverConn) handleStreams() {
	var strms Streams

	for {
		select {
		case fr, ok := <-sc.reader:
			if !ok {
				return
			}

			strm := strms.Get(fr.Stream())
			if strm == nil { // then create it
				strm = NewStream(fr.Stream(), int(sc.clientStreamWindow))
				strms.Insert(strm)

				sc.adpr.OnNewStream(strm)
			}

			if err := sc.adpr.OnFrame(strm, fr, sc.dec); err != nil {
				println("error")
				writeError(strm, err, sc.writer)
			}

			handleState(fr, strm)

			switch strm.State() {
			case StreamStateHalfClosed:
				sc.adpr.OnRequestFinished(strm, sc.enc, sc.writer)
				fallthrough
			case StreamStateClosed:
				sc.adpr.OnStreamEnd(strm)
				strms.Del(strm.ID())
			}
		}
	}
}

func (sc *serverConn) writeLoop() {
	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

loop:
	for {
		select {
		case fr, ok := <-sc.writer:
			if !ok {
				break loop
			}

			_, err := fr.WriteTo(sc.bw)
			if err == nil {
				err = sc.bw.Flush()
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
