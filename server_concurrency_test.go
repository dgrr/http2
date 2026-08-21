package http2

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// newConcurrencyServer starts a server with a handler of the test's choosing.
// newAttackServer fixes the handler, and these tests need one that blocks.
func newConcurrencyServer(t *testing.T, cnf ServerConfig, h fasthttp.RequestHandler) string {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: h,
		Logger:  discardLogger{},
	}
	ConfigureServer(server, cnf)

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

// TestServerHandlesStreamsConcurrently checks that streams multiplexed onto one
// connection are handled at the same time rather than one after another.
//
// The handler waits for every request to arrive before any of them returns, so
// a server that dispatches them serially cannot get past the first one and the
// barrier times out. Multiplexing exists so that a slow request does not hold
// up the ones behind it, and a server that runs handlers inline on its frame
// loop gives none of that back.
func TestServerHandlesStreamsConcurrently(t *testing.T) {
	const streams = 8

	var arrived sync.WaitGroup
	arrived.Add(streams)

	release := make(chan struct{})

	var timedOut atomic.Bool

	addr := newConcurrencyServer(t, ServerConfig{PingInterval: -1},
		func(ctx *fasthttp.RequestCtx) {
			arrived.Done()

			select {
			case <-release:
			case <-time.After(5 * time.Second):
				timedOut.Store(true)
			}

			ctx.SetBodyString("ok")
		})

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	var wg sync.WaitGroup

	for i := 0; i < streams; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			req := fasthttp.AcquireRequest()
			res := fasthttp.AcquireResponse()

			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(res)

			req.SetRequestURI("https://" + addr + "/")

			if err := hc.Do(req, res); err != nil {
				t.Errorf("request: %v", err)
				return
			}

			if res.StatusCode() != fasthttp.StatusOK {
				t.Errorf("status = %d, want 200", res.StatusCode())
			}
		}()
	}

	// All of them have to be inside the handler at once for this to return.
	done := make(chan struct{})
	go func() {
		arrived.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		wg.Wait()
		t.Fatal("only some of the streams reached the handler: the server is " +
			"handling one request at a time per connection")
	}

	close(release)
	wg.Wait()

	if timedOut.Load() {
		t.Error("a handler gave up waiting for the others")
	}
}

// TestConcurrentStreamsRespectMaxConcurrent checks the other side of the same
// coin: handlers run at once, but never more of them than the connection
// advertised in SETTINGS_MAX_CONCURRENT_STREAMS.
//
// It drives one raw connection rather than the client, because the client
// opens a second connection when the first runs out of streams and the total
// across connections says nothing about the per-connection limit.
func TestConcurrentStreamsRespectMaxConcurrent(t *testing.T) {
	const (
		maxStreams = 8
		requests   = 64
	)

	var inFlight, peak atomic.Int64

	release := make(chan struct{})

	addr := newConcurrencyServer(t,
		ServerConfig{PingInterval: -1, MaxConcurrentStreams: maxStreams},
		func(ctx *fasthttp.RequestCtx) {
			trackPeak(&inFlight, &peak)
			defer inFlight.Add(-1)

			<-release

			ctx.SetBodyString("ok")
		})

	a := dialAttacker(t, addr)
	d := a.drain()

	fields := requestFields(addr)

	for i := 0; i < requests; i++ {
		if a.writeHeaders(uint32(i*2+1), true, true, fields) != nil {
			break
		}
	}

	_ = a.bw.Flush()

	// Let the server take as many of them as it is willing to.
	time.Sleep(500 * time.Millisecond)

	got := peak.Load()

	close(release)
	d.wait(2 * time.Second)

	if got > maxStreams {
		t.Errorf("peak concurrent handlers = %d, want at most %d", got, maxStreams)
	}

	if got < 2 {
		t.Errorf("peak concurrent handlers = %d, want the handlers to overlap", got)
	}
}

// trackPeak bumps the in-flight counter and records the high-water mark.
func trackPeak(inFlight, peak *atomic.Int64) {
	n := inFlight.Add(1)

	for {
		p := peak.Load()
		if n <= p || peak.CompareAndSwap(p, n) {
			return
		}
	}
}

// TestRapidResetBoundsHandlers drives the amplification half of
// CVE-2023-44487. The attacker sends a complete request, so the handler does
// start, and cancels it in the same breath. If canceling gives the concurrency
// slot straight back, every RST_STREAM buys another handler, and the peer can
// have unbounded work running at once while never appearing to exceed
// SETTINGS_MAX_CONCURRENT_STREAMS.
//
// The fix is to hold the slot until the handler it belongs to has actually
// returned, which is what this measures.
func TestRapidResetBoundsHandlers(t *testing.T) {
	const (
		maxStreams = 16
		attempts   = 5000
	)

	var inFlight, peak, started atomic.Int64

	release := make(chan struct{})

	addr := newConcurrencyServer(t,
		ServerConfig{PingInterval: -1, MaxConcurrentStreams: maxStreams},
		func(ctx *fasthttp.RequestCtx) {
			started.Add(1)

			trackPeak(&inFlight, &peak)
			defer inFlight.Add(-1)

			<-release

			ctx.SetBodyString("ok")
		})

	a := dialAttacker(t, addr)
	d := a.drain()

	fields := requestFields(addr)

	sent := 0

	for i := 0; i < attempts; i++ {
		id := uint32(i*2 + 1)

		// END_STREAM: the request is complete, so the handler runs. The reset
		// follows immediately, before the response can be written.
		if a.writeHeaders(id, true, true, fields) != nil {
			break
		}

		if a.writeRST(id, StreamCanceled) != nil {
			break
		}

		sent++

		if i%64 == 0 && a.bw.Flush() != nil {
			break
		}
	}

	_ = a.bw.Flush()

	// Let the server work through the backlog with every handler still parked.
	time.Sleep(time.Second)

	got := peak.Load()

	close(release)
	d.wait(2 * time.Second)

	t.Logf("sent %d canceled requests, %d handlers started, peak %d concurrent",
		sent, started.Load(), got)

	if got > maxStreams {
		t.Errorf("peak concurrent handlers = %d over %d canceled requests, want at most %d",
			got, sent, maxStreams)
	}
}

// TestHandlerPanicDoesNotKillTheProcess covers the handler running on its own
// goroutine: a panic there has no caller to recover it, so it has to be caught
// where it happens or it takes the whole process down.
func TestHandlerPanicDoesNotKillTheProcess(t *testing.T) {
	addr := newConcurrencyServer(t, ServerConfig{PingInterval: -1},
		func(ctx *fasthttp.RequestCtx) {
			if string(ctx.Path()) == "/panic" {
				panic("handler blew up")
			}

			ctx.SetBodyString("ok")
		})

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	do := func(path string) (int, error) {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + path)

		if err := hc.Do(req, res); err != nil {
			return 0, err
		}

		return res.StatusCode(), nil
	}

	code, err := do("/panic")
	if err != nil {
		t.Fatalf("request to a panicking handler: %v", err)
	}

	if code != fasthttp.StatusInternalServerError {
		t.Errorf("status = %d, want %d", code, fasthttp.StatusInternalServerError)
	}

	// The connection has to survive it, the same way it would in fasthttp.
	for i := 0; i < 3; i++ {
		code, err := do(fmt.Sprintf("/after-%d", i))
		if err != nil {
			t.Fatalf("request %d after the panic: %v", i, err)
		}

		if code != fasthttp.StatusOK {
			t.Errorf("request %d after the panic: status = %d, want 200", i, code)
		}
	}
}
