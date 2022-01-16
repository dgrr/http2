package http2

import (
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttputil"
)

func serve(s *Server, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			break
		}

		go s.ServeConn(c)
	}
}

func getConn(s *Server) (*Conn, net.Listener, error) {
	s.cnf.defaults()

	ln := fasthttputil.NewInmemoryListener()

	go serve(s, ln)

	c, err := ln.Dial()
	if err != nil {
		return nil, nil, err
	}

	nc := NewConn(c, ConnOpts{})

	return nc, ln, nc.doHandshake()
}

func makeHeaders(id uint32, enc *HPACK, endHeaders, endStream bool, hs map[string]string) *FrameHeader {
	fr := AcquireFrameHeader()

	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()

	for k, v := range hs {
		hf.Set(k, v)
		enc.AppendHeaderField(h, hf, k[0] == ':')
	}

	h.SetPadding(false)
	h.SetEndStream(endStream)
	h.SetEndHeaders(endHeaders)

	return fr
}

func TestIssue52(t *testing.T) {
	for i := 0; i < 100; i++ {
		testIssue52(t)
	}
}

func testIssue52(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				io.WriteString(ctx, "Hello world")
			},
			ReadTimeout: time.Second * 30,
		},
		cnf: ServerConfig{
			Debug: false,
		},
	}

	c, ln, err := getConn(s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer ln.Close()

	msg := []byte("Hello world, how are you doing?")

	h1 := makeHeaders(3, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(9, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h3 := makeHeaders(7, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
	})
	h4 := makeHeaders(11, c.enc, true, true, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
	})

	c.writeFrame(h1)
	c.writeFrame(h2)
	c.writeFrame(h3)
	c.writeFrame(h4)

	for _, h := range []*FrameHeader{h1, h2} {
		err = writeData(c.bw, h, msg)
		if err != nil {
			t.Fatal(err)
		}

		c.bw.Flush()
	}

	// expect [GOAWAY, RESET, HEADERS, DATA, HEADERS, DATA]
	expect := []FrameType{
		FrameGoAway, FrameResetStream, FrameHeaders,
		FrameData, FrameHeaders, FrameData,
	}

	for len(expect) != 0 {
		next := expect[0]

		fr, err := c.readNext()
		if err != nil {
			t.Fatal(err)
		}

		if fr.Type() != next {
			t.Fatalf("unexpected frame type: %s <> %s", next, fr.Type())
		}

		if fr.Type() == FrameResetStream {
			rst := fr.Body().(*RstStream)
			if rst.Code() != RefusedStreamError {
				t.Fatalf("expected RefusedStreamError, got %s", rst.Code())
			}
		}

		expect = expect[1:]
	}

	_, err = c.readNext()
	if err == nil {
		t.Fatal("Expecting error")
	}

	if err != io.EOF {
		t.Fatalf("expected EOF, got %s", err)
	}
}

func TestIssue27(t *testing.T) {
	s := &Server{
		s: &fasthttp.Server{
			Handler: func(ctx *fasthttp.RequestCtx) {
				io.WriteString(ctx, "Hello world")
			},
			ReadTimeout: time.Second * 1,
		},
		cnf: ServerConfig{
			Debug: false,
		},
	}

	c, ln, err := getConn(s)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	defer ln.Close()

	msg := []byte("Hello world, how are you doing?")

	h1 := makeHeaders(3, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h2 := makeHeaders(5, c.enc, true, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "POST",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})
	h3 := makeHeaders(7, c.enc, false, false, map[string]string{
		string(StringAuthority): "localhost",
		string(StringMethod):    "GET",
		string(StringPath):      "/hello/world",
		string(StringScheme):    "https",
		"Content-Length":        strconv.Itoa(len(msg)),
	})

	c.writeFrame(h1)
	c.writeFrame(h2)

	time.Sleep(time.Second)
	c.writeFrame(h3)

	id := uint32(3)

	for i := 0; i < 3; i++ {
		fr, err := c.readNext()
		if err != nil {
			t.Fatal(err)
		}

		if fr.Stream() != id {
			t.Fatalf("Expecting update on stream %d, got %d", id, fr.Stream())
		}

		if fr.Type() != FrameResetStream {
			t.Fatalf("Expecting Reset, got %s", fr.Type())
		}

		rst := fr.Body().(*RstStream)
		if rst.Code() != StreamCanceled {
			t.Fatalf("Expecting StreamCanceled, got %s", rst.Code())
		}

		id += 2
	}
}
