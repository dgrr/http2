package http2

import (
	"crypto/tls"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A client that sends more than the peer's window allows gets its connection
// killed with FLOW_CONTROL_ERROR by anything that enforces the rule, so these
// tests pin how much goes out before a WINDOW_UPDATE arrives.
// https://httpwg.org/specs/rfc7540.html#rfc.section.6.9

// countData reads frames until the connection goes quiet and reports how many
// DATA bytes arrived.
func countData(p *peer, quiet time.Duration) int {
	total := 0

	for {
		_ = p.c.SetReadDeadline(time.Now().Add(quiet))

		fr, err := ReadFrameFrom(p.br)
		if err != nil {
			return total
		}

		if fr.Type() == FrameData {
			total += fr.Len()
		}

		ReleaseFrameHeader(fr)
	}
}

// postAsync starts a POST and hands back a channel carrying its error.
func postAsync(hc *fasthttp.HostClient, addr string, body []byte) <-chan error {
	done := make(chan error, 1)

	go func() {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetBody(body)

		done <- hc.Do(req, res)
	}()

	return done
}

// TestClientStopsAtConnectionWindow checks the client stops at the 65535 byte
// connection window every endpoint starts with, rather than emptying the whole
// body onto the wire.
func TestClientStopsAtConnectionWindow(t *testing.T) {
	got := make(chan int, 1)

	addr := newRawServerSettings(t, func(st *Settings) {
		// A large stream window leaves the connection window as the only limit.
		st.SetMaxWindowSize(1 << 20)
	}, func(p *peer) {
		select {
		case got <- countData(p, time.Second):
		default:
		}
	})

	hc := clientFor(t, addr)

	_ = postAsync(hc, addr, make([]byte, 1<<20))

	select {
	case n := <-got:
		if n != int(defaultWindowSize) {
			t.Errorf("client sent %d bytes, want %d (the connection window)", n, defaultWindowSize)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server never saw the request")
	}
}

// TestClientStopsAtStreamWindow checks SETTINGS_INITIAL_WINDOW_SIZE is applied
// per stream, not ignored in favor of the connection window.
func TestClientStopsAtStreamWindow(t *testing.T) {
	const streamWindow = 4096

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

	_ = postAsync(hc, addr, make([]byte, 1<<20))

	select {
	case n := <-got:
		if n != streamWindow {
			t.Errorf("client sent %d bytes, want %d (the stream window)", n, streamWindow)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server never saw the request")
	}
}

// TestClientResumesAfterWindowUpdate checks the held back tail of a body goes
// out once the peer opens its windows, and that the request then completes.
func TestClientResumesAfterWindowUpdate(t *testing.T) {
	const body = 256 << 10

	got := make(chan int, 1)

	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()
		if id == 0 {
			return
		}

		total := 0

		for total < body {
			_ = p.c.SetReadDeadline(time.Now().Add(5 * time.Second))

			fr, err := ReadFrameFrom(p.br)
			if err != nil {
				break
			}

			if fr.Type() == FrameData {
				total += fr.Len()

				// Hand back exactly what was used, on both levels, the way a
				// server that is keeping up would.
				p.writeWindowUpdate(0, uint32(fr.Len()))
				p.writeWindowUpdate(id, uint32(fr.Len()))
			}

			ReleaseFrameHeader(fr)
		}

		select {
		case got <- total:
		default:
		}

		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	errCh := postAsync(hc, addr, make([]byte, body))

	select {
	case n := <-got:
		if n != body {
			t.Errorf("server received %d of %d body bytes", n, body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the body never finished arriving")
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("request failed: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("request never returned")
	}
}

// TestClientAppliesInitialWindowChange covers RFC 7540 6.9.2: a SETTINGS frame
// that changes SETTINGS_INITIAL_WINDOW_SIZE mid-connection shifts the window of
// every stream that is already open.
func TestClientAppliesInitialWindowChange(t *testing.T) {
	const (
		firstWindow = 4096
		extra       = 4096
	)

	got := make(chan int, 1)

	addr := newRawServerSettings(t, func(st *Settings) {
		st.SetMaxWindowSize(firstWindow)
	}, func(p *peer) {
		// Wait for the client to run out of stream window.
		total := 0

		for total < firstWindow {
			_ = p.c.SetReadDeadline(time.Now().Add(5 * time.Second))

			fr, err := ReadFrameFrom(p.br)
			if err != nil {
				return
			}

			if fr.Type() == FrameData {
				total += fr.Len()
			}

			ReleaseFrameHeader(fr)
		}

		// Raising the setting has to move the open stream along with it.
		fr := AcquireFrameHeader()

		st := AcquireFrame(FrameSettings).(*Settings)
		st.Reset()
		st.SetMaxWindowSize(firstWindow + extra)

		fr.SetBody(st)

		_, _ = fr.WriteTo(p.bw)

		ReleaseFrameHeader(fr)

		_ = p.bw.Flush()

		select {
		case got <- total + countData(p, time.Second):
		default:
		}
	})

	hc := clientFor(t, addr)

	_ = postAsync(hc, addr, make([]byte, 1<<20))

	select {
	case n := <-got:
		if n != firstWindow+extra {
			t.Errorf("client sent %d bytes, want %d after the window was raised", n, firstWindow+extra)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server never saw the request")
	}
}

// TestClientCloseUnderLoad closes a client while requests are in flight. The
// write loop used to be shut down by closing the channel requests arrive on,
// which panicked the whole process whenever a caller was mid-send.
func TestClientCloseUnderLoad(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) { ctx.SetBodyString("ok") },
		Logger:  discardLogger{},
	}
	ConfigureServer(server, ServerConfig{PingInterval: -1})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	for round := 0; round < 60; round++ {
		hc := &fasthttp.HostClient{
			Addr:      addr,
			IsTLS:     true,
			TLSConfig: &tls.Config{InsecureSkipVerify: true},
		}

		if err := ConfigureClient(hc, ClientOpts{
			PingInterval:    -1,
			MaxResponseTime: 10 * time.Second,
		}); err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup

		for i := 0; i < 16; i++ {
			wg.Add(1)

			go func() {
				defer wg.Done()

				req := fasthttp.AcquireRequest()
				res := fasthttp.AcquireResponse()

				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(res)

				req.SetRequestURI("https://" + addr + "/")

				// Either outcome is fine. Hanging or panicking is not.
				_ = hc.Do(req, res)
			}()
		}

		// Spread the close across the window where requests are being queued.
		time.Sleep(time.Duration(round%40) * 50 * time.Microsecond)

		_ = ClientFrom(hc).Close()

		done := make(chan struct{})

		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("requests did not finish after the client was closed")
		}
	}
}

// TestClientStopsOpeningStreamsAfterGoAway checks a connection the server has
// said goodbye to is not handed any more requests.
func TestClientStopsOpeningStreamsAfterGoAway(t *testing.T) {
	c := &Conn{
		nextID:     1,
		maxStreams: 100,
		done:       make(chan struct{}),
	}

	if !c.CanOpenStream() {
		t.Fatal("a fresh connection should accept streams")
	}

	atomic.StoreUint32(&c.goAway, 1)

	if c.CanOpenStream() {
		t.Error("connection still accepting streams after GOAWAY")
	}
}

// TestClientStreamIDExhaustion checks the client refuses to wrap past the
// largest legal stream id instead of reusing identifiers with the reserved bit
// set, which the peer would read as a stream it has already closed.
// https://httpwg.org/specs/rfc7540.html#rfc.section.5.1.1
func TestClientStreamIDExhaustion(t *testing.T) {
	c := &Conn{
		nextID:     maxStreamID,
		maxStreams: 100,
		done:       make(chan struct{}),
	}

	if !c.CanOpenStream() {
		t.Fatal("the last usable stream id should still be offered")
	}

	atomic.StoreUint32(&c.nextID, maxStreamID+2)

	if c.CanOpenStream() {
		t.Error("connection offered a stream id past the end of the space")
	}

	ctx := &Ctx{
		Request:  fasthttp.AcquireRequest(),
		Response: fasthttp.AcquireResponse(),
		Err:      make(chan error, 1),
	}

	defer fasthttp.ReleaseRequest(ctx.Request)
	defer fasthttp.ReleaseResponse(ctx.Response)

	// CanOpenStream is the guard callers see, but writeRequest has to hold the
	// line too: a Ctx can be queued before the last id is taken.
	if err := c.writeRequest(ctx); err == nil {
		t.Error("writeRequest handed out a stream id past the end of the space")
	}
}

// TestServerRefillsReceiveWindow uploads several times the server's receive
// window over one connection. The server used to consume its window without
// ever sending a WINDOW_UPDATE back, so a client that respects flow control
// (which is every real one) stopped uploading for good once the initial window
// was spent, part way through this test.
func TestServerRefillsReceiveWindow(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyString(strconv.Itoa(len(ctx.Request.Body())))
		},
		Logger: discardLogger{},
	}
	ConfigureServer(server, ServerConfig{PingInterval: -1})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{
		PingInterval:    -1,
		MaxResponseTime: 15 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	// The server opens with a 4 MiB connection window, so this is several
	// windows worth on a single connection.
	const (
		body   = 256 << 10
		rounds = 64
	)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodPost)

	payload := make([]byte, body)

	for i := 0; i < rounds; i++ {
		req.SetBody(payload)

		if err := hc.Do(req, res); err != nil {
			t.Fatalf("upload %d of %d (%d bytes sent so far): %v", i+1, rounds, i*body, err)
		}

		if got := string(res.Body()); got != strconv.Itoa(body) {
			t.Fatalf("upload %d: server saw %s bytes, want %d", i+1, got, body)
		}
	}
}

// TestServerLimitsRequestBody checks the server refuses a body larger than the
// fasthttp server is configured to accept, both when the size is declared up
// front and when the peer just keeps sending. The body is buffered in memory,
// so without this one stream can grow until the process dies.
func TestServerLimitsRequestBody(t *testing.T) {
	const limit = 64 << 10

	certPEM, keyPEM := testKeyPair(t)

	var served atomic.Int64

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			served.Add(1)
			ctx.SetBodyString("ok")
		},
		MaxRequestBodySize: limit,
		Logger:             discardLogger{},
	}
	ConfigureServer(server, ServerConfig{PingInterval: -1})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{
		PingInterval:    -1,
		MaxResponseTime: 10 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	post := func(n int) error {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodPost)
		req.SetBody(make([]byte, n))

		return hc.Do(req, res)
	}

	if err := post(limit); err != nil {
		t.Fatalf("a body at the limit was refused: %v", err)
	}

	before := served.Load()

	if err := post(limit * 4); err == nil {
		t.Error("a body four times the limit was accepted")
	}

	if served.Load() != before {
		t.Error("the handler ran on a request whose body was over the limit")
	}
}
