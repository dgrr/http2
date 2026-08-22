package http2

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A request body set with SetBodyStream has to go out a frame at a time as the
// peer's windows allow, the way the server already does for responses. Reading
// it into memory first defeats the point: the caller reaches for a stream
// precisely when the body is too big to hold.

// countingReader hands out a fixed body and records how much has been read, so
// a test can tell streaming from buffering: a client that reads the whole
// reader up front has buffered it whatever it does with the bytes afterwards.
type countingReader struct {
	data   []byte
	off    int
	read   atomic.Int64
	closed atomic.Bool
}

func newCountingReader(n int) *countingReader {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte('a' + i%26)
	}

	return &countingReader{data: data}
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}

	n := copy(p, r.data[r.off:])
	r.off += n

	r.read.Add(int64(n))

	return n, nil
}

func (r *countingReader) Close() error {
	r.closed.Store(true)
	return nil
}

// echoServer answers with the request body it received, so a test can check
// every byte survived the trip.
func echoServer(t *testing.T) string {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("x-body-len", fmt.Sprint(len(ctx.Request.Body())))
			ctx.Response.Header.Set("x-transfer-encoding",
				string(ctx.Request.Header.Peek("Transfer-Encoding")))
			ctx.SetBody(ctx.Request.Body())
		},
		Logger:             discardLogger{},
		MaxRequestBodySize: 16 << 20,
	}
	ConfigureServer(server, ServerConfig{PingInterval: -1})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()
	waitForServer(t, addr)

	return addr
}

func TestClientSendsStreamedBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{name: "declared length", size: 0},
		{name: "unknown length", size: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const bodyLen = 512 << 10

			addr := echoServer(t)
			hc := clientFor(t, addr)

			r := newCountingReader(bodyLen)

			size := tc.size
			if size == 0 {
				size = bodyLen
			}

			req := fasthttp.AcquireRequest()
			res := fasthttp.AcquireResponse()

			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(res)

			req.SetRequestURI("https://" + addr + "/")
			req.Header.SetMethod(fasthttp.MethodPost)
			req.SetBodyStream(r, size)

			if err := hc.Do(req, res); err != nil {
				t.Fatalf("request: %v", err)
			}

			if res.StatusCode() != fasthttp.StatusOK {
				t.Fatalf("status = %d, want 200", res.StatusCode())
			}

			if got := string(res.Header.Peek("x-body-len")); got != fmt.Sprint(bodyLen) {
				t.Errorf("server received %s bytes, want %d", got, bodyLen)
			}

			if !bytes.Equal(res.Body(), r.data) {
				t.Errorf("the body came back changed: %d bytes, want %d",
					len(res.Body()), len(r.data))
			}

			// Transfer-Encoding is connection specific and has no meaning in
			// HTTP/2, where END_STREAM ends the body. fasthttp sets it on a
			// stream of unknown length for the HTTP/1 writer's benefit.
			if got := string(res.Header.Peek("x-transfer-encoding")); got != "" {
				t.Errorf("server saw Transfer-Encoding: %q, want it stripped", got)
			}
		})
	}
}

// TestClientStreamedBodyStopsAtWindow is the test that tells streaming from
// buffering. The peer opens a small window and never grows it, so a client that
// meters the body against the window can only have pulled about that much out
// of the reader.
func TestClientStreamedBodyStopsAtWindow(t *testing.T) {
	const (
		streamWindow = 16 << 10
		bodyLen      = 4 << 20
	)

	got := make(chan int, 1)

	addr := newRawServerSettings(t, func(st *Settings) {
		st.SetMaxWindowSize(streamWindow)
	}, func(p *peer) {
		select {
		case got <- countData(p, time.Second):
		default:
		}
	})

	hc := clientFor(t, addr)

	r := newCountingReader(bodyLen)

	go func() {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetBodyStream(r, bodyLen)

		_ = hc.Do(req, res)
	}()

	select {
	case n := <-got:
		if n != streamWindow {
			t.Errorf("client sent %d bytes, want %d (the stream window)", n, streamWindow)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server never saw the request")
	}

	// One frame of slack: the client may hold a chunk it has read but cannot
	// send yet. What must not happen is the whole 4 MiB being pulled in.
	if read := r.read.Load(); read > streamWindow+(1<<14) {
		t.Errorf("client read %d bytes out of the reader against a %d byte window, "+
			"want it pulled a frame at a time rather than buffered", read, streamWindow)
	}
}

// TestClientStreamedBodyResumesAfterWindowUpdate checks the held back tail goes
// out once the peer opens its windows, and that the request then completes.
func TestClientStreamedBodyResumesAfterWindowUpdate(t *testing.T) {
	const bodyLen = 256 << 10

	addr := newRawServerSettings(t, func(st *Settings) {
		st.SetMaxWindowSize(4096)
	}, func(p *peer) {
		id := p.waitForRequest()

		total := 0

		for total < bodyLen {
			_ = p.c.SetReadDeadline(time.Now().Add(10 * time.Second))

			fr, err := ReadFrameFrom(p.br)
			if err != nil {
				return
			}

			if fr.Type() == FrameData {
				total += fr.Len()

				p.writeWindowUpdate(0, uint32(fr.Len()))
				p.writeWindowUpdate(id, uint32(fr.Len()))
			}

			ReleaseFrameHeader(fr)
		}

		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	r := newCountingReader(bodyLen)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyStream(r, bodyLen)

	if err := hc.Do(req, res); err != nil {
		t.Fatalf("request: %v", err)
	}

	if res.StatusCode() != fasthttp.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode())
	}

	if read := r.read.Load(); read != bodyLen {
		t.Errorf("client read %d of %d body bytes", read, bodyLen)
	}
}

// TestClientClosesStreamedBody covers the reader's Close. fasthttp closes it
// itself when it writes the body over HTTP/1, so whatever is behind it, a file
// or a pipe, stays open here unless this connection does the same.
func TestClientClosesStreamedBody(t *testing.T) {
	addr := echoServer(t)
	hc := clientFor(t, addr)

	r := newCountingReader(64 << 10)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyStream(r, 64<<10)

	if err := hc.Do(req, res); err != nil {
		t.Fatalf("request: %v", err)
	}

	if !r.closed.Load() {
		t.Error("the request body stream was left open")
	}
}

// TestClientSendsEmptyStreamedBody covers a stream that turns out to have
// nothing in it. The END_STREAM flag has already been left off the HEADERS
// frame by then, so an empty DATA frame has to carry it or the request never
// ends.
func TestClientSendsEmptyStreamedBody(t *testing.T) {
	addr := echoServer(t)
	hc := clientFor(t, addr)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyStream(newCountingReader(0), -1)

	done := make(chan error, 1)
	go func() { done <- hc.Do(req, res) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("request: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished: END_STREAM was never sent")
	}

	if got := string(res.Header.Peek("x-body-len")); got != "0" {
		t.Errorf("server received %s body bytes, want 0", got)
	}
}

// TestClientStreamedBodyDoesNotBuffer checks the whole point of the feature:
// sending a body far larger than memory should cost roughly one frame of it.
//
// The peer is a raw one that throws every DATA frame away. A server built on
// this library would buffer the request body into its own RequestCtx, and
// since it runs in this process that 64 MiB would land in the same heap
// measurement and say nothing about the client.
func TestClientStreamedBodyDoesNotBuffer(t *testing.T) {
	if testing.Short() {
		t.Skip("sends a large body")
	}

	const bodyLen = 64 << 20

	addr := newRawServerSettings(t, func(st *Settings) {
		st.SetMaxWindowSize(1 << 20)
	}, func(p *peer) {
		id := p.waitForRequest()

		for {
			_ = p.c.SetReadDeadline(time.Now().Add(20 * time.Second))

			fr, err := ReadFrameFrom(p.br)
			if err != nil {
				return
			}

			end := fr.Type() == FrameData && fr.Flags().Has(FlagEndStream)

			if fr.Type() == FrameData && fr.Len() > 0 {
				p.writeWindowUpdate(0, uint32(fr.Len()))
				p.writeWindowUpdate(id, uint32(fr.Len()))
			}

			ReleaseFrameHeader(fr)

			if end {
				break
			}
		}

		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodPost)
	req.SetBodyStream(io.LimitReader(zeroes{}, bodyLen), bodyLen)

	grew := heapDelta(func() {
		if err := hc.Do(req, res); err != nil {
			t.Fatalf("request: %v", err)
		}
	})

	t.Logf("streaming %d MiB grew the heap by %.2f MiB", bodyLen>>20, float64(grew)/(1<<20))

	// Generous: the point is that it is not the 64 MiB a buffered body costs.
	if grew > 8<<20 {
		t.Errorf("streaming a %d MiB body grew the heap by %d bytes, want the "+
			"body not to be held in memory", bodyLen>>20, grew)
	}
}

// zeroes is an endless body that costs nothing to produce.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }
