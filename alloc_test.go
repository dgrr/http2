// The race detector allocates its own bookkeeping on top of everything this
// measures, so the counts here only mean anything without it.
//go:build !race

package http2

import (
	"crypto/tls"
	"net"
	"runtime"
	"testing"

	"github.com/valyala/fasthttp"
)

// TestAllocsPerRequest puts a ceiling on the work a request does that the GC
// has to clean up afterwards. Both ends run in this process, so the count
// covers the client, the server and the TLS layer between them.
//
// This started as a regression test for two bugs that never showed up as a
// failure anywhere else: the server dropped every stream frame it read on the
// floor instead of returning it to the pool, and the client allocated a
// context, a channel and a timer for every request.
func TestAllocsPerRequest(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetContentType("text/plain")
			ctx.SetBodyString("hello")
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

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")

	do := func(n int) {
		t.Helper()

		for i := 0; i < n; i++ {
			if err := hc.Do(req, res); err != nil {
				t.Fatalf("request %d: %v", i, err)
			}
		}
	}

	// Warm every pool on both sides first, so the count is the steady state
	// rather than the cost of starting up.
	do(2000)

	const n = 20000

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	do(n)

	runtime.ReadMemStats(&after)

	perRequest := float64(after.Mallocs-before.Mallocs) / n

	// Around 2.5 with everything pooled. The ceiling leaves room for the
	// runtime and the TLS layer to move without failing the build, while still
	// catching anything that starts allocating per request or per frame.
	const ceiling = 10

	t.Logf("%.2f allocations per request", perRequest)

	if perRequest > ceiling {
		t.Errorf("%.2f allocations per request, want at most %d", perRequest, ceiling)
	}
}
