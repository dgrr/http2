package http2

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// TestRequestRacingCloseIsRetryable drives requests straight at RoundTrip
// while connections are closed underneath them, and checks that a request the
// connection never carried is reported as retryable. That is what lets
// fasthttp put it on a fresh connection instead of handing the caller an error
// for something no code of theirs did wrong.
func TestRequestRacingCloseIsRetryable(t *testing.T) {
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

	cl := ClientFrom(hc)

	t.Cleanup(func() { _ = cl.Close() })

	var (
		wg       sync.WaitGroup
		notReady atomic.Int64
	)

	for round := 0; round < 40; round++ {
		for i := 0; i < 8; i++ {
			wg.Add(1)

			go func() {
				defer wg.Done()

				req := fasthttp.AcquireRequest()
				res := fasthttp.AcquireResponse()

				defer fasthttp.ReleaseRequest(req)
				defer fasthttp.ReleaseResponse(res)

				req.SetRequestURI("https://" + addr + "/")
				req.Header.SetMethod(fasthttp.MethodGet)

				retry, err := cl.RoundTrip(hc, req, res)
				if err == nil {
					return
				}

				if errors.Is(err, ErrConnectionClosed) {
					notReady.Add(1)

					if !retry {
						t.Errorf("a request the connection never carried was not retryable: %v", err)
					}
				}
			}()
		}

		// Close whatever the client is holding, out from under the requests
		// that are being queued right now.
		cl.lck.Lock()

		conns := make([]*Conn, 0, cl.conns.Len())
		for e := cl.conns.Front(); e != nil; e = e.Next() {
			conns = append(conns, e.Value.(*Conn))
		}

		cl.lck.Unlock()

		for _, conn := range conns {
			_ = conn.Close()
		}
	}

	wg.Wait()

	t.Logf("%d requests were closed out before they reached the server", notReady.Load())
}

// TestRetryableClassification pins which failures mean the request never
// started. Reporting anything else as retryable would resend a request the
// server may already have acted on.
func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrConnectionClosed, true},
		{ErrNotAvailableStreams, true},
		{ErrNoMoreStreamIDs, true},
		{ErrRequestCanceled, false},
		{ErrClientClosed, false},
		{ErrTimeout, false},
		{errors.New("some transport failure"), false},
		{NewResetStreamError(StreamCanceled, "reset"), false},
	}

	for _, tc := range cases {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestClientDoesNotDialForever checks a server that will never accept a stream
// is reported as an error rather than dialed at until something gives.
func TestClientDoesNotDialForever(t *testing.T) {
	addr := newRawServerSettings(t, func(st *Settings) {
		st.SetMaxConcurrentStreams(0)
	}, func(p *peer) {
		// Read whatever turns up and answer nothing.
		for p.readFrame() != nil { //nolint:revive
		}
	})

	hc := clientFor(t, addr)

	done := make(chan error, 1)

	go func() {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")

		done <- hc.Do(req, res)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("request succeeded against a server that accepts no streams")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the client never gave up")
	}
}

// TestTimedOutRequestsDoNotPoisonTheConnection covers what happens after a
// request gives up waiting. Canceling used to leave the stream in the
// connection's table with openStreams still counting it, so a connection that
// saw enough timeouts stopped accepting requests for good while looking
// perfectly healthy.
func TestTimedOutRequestsDoNotPoisonTheConnection(t *testing.T) {
	// A server that answers nothing, so every request hits MaxResponseTime.
	silent := make(chan struct{})

	addr := newRawServer(t, func(p *peer) {
		<-silent
	})

	t.Cleanup(func() { close(silent) })

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{
		PingInterval:    -1,
		MaxResponseTime: 50 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	cl := ClientFrom(hc)

	t.Cleanup(func() { _ = cl.Close() })

	const timeouts = 20

	for i := 0; i < timeouts; i++ {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		req.SetRequestURI("https://" + addr + "/")

		if err := hc.Do(req, res); err == nil {
			t.Fatalf("request %d succeeded against a silent server", i)
		}

		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(res)
	}

	cl.lck.Lock()

	conns := make([]*Conn, 0, cl.conns.Len())
	for e := cl.conns.Front(); e != nil; e = e.Next() {
		conns = append(conns, e.Value.(*Conn))
	}

	cl.lck.Unlock()

	if len(conns) == 0 {
		t.Fatal("the client kept no connection")
	}

	for _, c := range conns {
		if n := atomic.LoadInt32(&c.openStreams); n != 0 {
			t.Errorf("connection still counts %d open streams after %d timeouts", n, timeouts)
		}

		c.reqLck.Lock()
		queued := len(c.reqQueued)
		c.reqLck.Unlock()

		if queued != 0 {
			t.Errorf("connection still holds %d timed-out requests", queued)
		}

		if !c.CanOpenStream() {
			t.Error("connection refuses new streams after nothing but timeouts")
		}
	}
}
