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

// TestStressManyClients opens one HTTP/2 connection per client against a single
// server and drives sequential requests down each of them at the same time.
//
// Every request carries a unique id in a header and the handler echoes it back
// in the body, so a response delivered to the wrong stream or the wrong
// connection fails the test instead of going unnoticed.
// stressPingInterval is short enough that both endpoints ping several times
// over the life of the test, so the PING/ACK frame handling is covered too.
const stressPingInterval = 100 * time.Millisecond

func TestStressManyClients(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in short mode")
	}

	const (
		clients           = 1000
		requestsPerClient = 100
	)

	certPEM, keyPEM := testKeyPair(t)

	// Every connection the server serves, recorded by remote address, so the
	// test can prove the load really was spread over `clients` connections
	// rather than multiplexed onto a handful of them.
	var conns sync.Map

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			conns.Store(ctx.RemoteAddr().String(), struct{}{})
			ctx.SetContentType("text/plain")
			ctx.SetBody(ctx.Request.Header.Peek("X-Req-Id"))
		},
		Concurrency: clients * 4,
	}

	// A ping interval well below the run time puts the PING/ACK path under the
	// same concurrency as the requests. At the default 10s it would never fire.
	ConfigureServer(server, ServerConfig{PingInterval: stressPingInterval})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	waitForServer(t, addr)

	var (
		ok       atomic.Int64
		failed   atomic.Int64
		firstErr atomic.Value
	)

	record := func(err error) {
		failed.Add(1)
		firstErr.CompareAndSwap(nil, err)
	}

	start := time.Now()

	var wg sync.WaitGroup

	wg.Add(clients)

	for c := 0; c < clients; c++ {
		go func(c int) {
			defer wg.Done()

			hc := &fasthttp.HostClient{
				Addr:      addr,
				IsTLS:     true,
				TLSConfig: &tls.Config{InsecureSkipVerify: true},
			}

			if err := ConfigureClient(hc, ClientOpts{
				PingInterval:    stressPingInterval,
				MaxResponseTime: 30 * time.Second,
			}); err != nil {
				for i := 0; i < requestsPerClient; i++ {
					record(fmt.Errorf("client %d: ConfigureClient: %w", c, err))
				}

				return
			}

			for i := 0; i < requestsPerClient; i++ {
				id := fmt.Sprintf("%d-%d", c, i)

				req := fasthttp.AcquireRequest()
				res := fasthttp.AcquireResponse()

				req.SetRequestURI("https://" + addr + "/echo")
				req.Header.SetMethod(fasthttp.MethodGet)
				req.Header.Set("X-Req-Id", id)

				switch err := hc.Do(req, res); {
				case err != nil:
					record(fmt.Errorf("client %d req %d: %w", c, i, err))
				case res.StatusCode() != fasthttp.StatusOK:
					record(fmt.Errorf("client %d req %d: status %d", c, i, res.StatusCode()))
				case string(res.Body()) != id:
					record(fmt.Errorf("client %d req %d: got body %q, want %q", c, i, res.Body(), id))
				default:
					ok.Add(1)
				}

				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(res)
			}
		}(c)
	}

	wg.Wait()

	elapsed := time.Since(start)
	total := int64(clients * requestsPerClient)

	t.Logf("%d clients x %d requests: %d ok, %d failed in %s (%.0f req/s)",
		clients, requestsPerClient, ok.Load(), failed.Load(), elapsed.Round(time.Millisecond),
		float64(total)/elapsed.Seconds())

	if n := failed.Load(); n != 0 {
		t.Fatalf("%d/%d requests failed, first error: %v", n, total, firstErr.Load())
	}

	// ConfigureClient only succeeds when ALPN negotiates "h2" (see conn.go), so
	// reaching here means every request above went over HTTP/2. What is left to
	// check is that they used one connection per client.
	var connCount int

	conns.Range(func(_, _ any) bool {
		connCount++
		return true
	})

	// The readiness probe in waitForServer contributes connections too, so the
	// count is a lower bound.
	if connCount < clients {
		t.Fatalf("server saw %d connections, want at least %d", connCount, clients)
	}
}

// waitForServer blocks until the TLS listener accepts an HTTP/2 connection.
func waitForServer(t *testing.T, addr string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		hc := &fasthttp.HostClient{
			Addr:      addr,
			IsTLS:     true,
			TLSConfig: &tls.Config{InsecureSkipVerify: true},
		}

		if err := ConfigureClient(hc, ClientOpts{}); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
