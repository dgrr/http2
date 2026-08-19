package http2

import (
	"crypto/tls"
	"runtime"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// Everything else in this package finishes in seconds and then exits, which
// hides anything that accumulates. These tests hold connections open across
// several ping intervals and cycle connections in waves, then check that
// goroutines and heap come back to where they started.

// settleTo waits for the goroutine count to come down to target and returns
// what it reached. Teardown is staggered across both ends of every connection
// plus fasthttp's worker pool, so a count that is merely stable for a moment is
// not the same as a count that has finished falling.
func settleTo(t *testing.T, target int, timeout time.Duration) int {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		runtime.GC()

		n := runtime.NumGoroutine()
		if n <= target || time.Now().After(deadline) {
			return n
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// TestSoakConnectionChurn opens and closes connections in waves. Every
// connection starts goroutines on both ends, so a connection that does not
// fully tear down shows up as goroutines that never come back.
func TestSoakConnectionChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in short mode")
	}

	const (
		waves           = 8
		connsPerWave    = 50
		requestsPerConn = 5
	)

	addr, _ := newAttackServer(t, ServerConfig{PingInterval: 100 * time.Millisecond})

	// Earlier tests in this binary may still be winding down, so wait for the
	// count to come down before taking it as the baseline.
	baseline := settleTo(t, 20, 30*time.Second)

	for w := 0; w < waves; w++ {
		clients := make([]*fasthttp.HostClient, 0, connsPerWave)

		for i := 0; i < connsPerWave; i++ {
			hc := &fasthttp.HostClient{
				Addr:      addr,
				IsTLS:     true,
				TLSConfig: &tls.Config{InsecureSkipVerify: true},
			}

			if err := ConfigureClient(hc, ClientOpts{
				PingInterval:    100 * time.Millisecond,
				MaxResponseTime: 10 * time.Second,
			}); err != nil {
				t.Fatalf("wave %d conn %d: %v", w, i, err)
			}

			clients = append(clients, hc)
		}

		for i, hc := range clients {
			for r := 0; r < requestsPerConn; r++ {
				req := fasthttp.AcquireRequest()
				res := fasthttp.AcquireResponse()

				req.SetRequestURI("https://" + addr + "/")
				req.Header.SetMethod(fasthttp.MethodGet)

				if err := hc.Do(req, res); err != nil {
					t.Fatalf("wave %d conn %d request %d: %v", w, i, r, err)
				}

				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(res)
			}
		}

		for _, hc := range clients {
			// HostClient.CloseIdleConnections does not reach HTTP/2
			// connections, so go through the library's own client.
			if err := ClientFrom(hc).Close(); err != nil {
				t.Errorf("wave %d: closing: %v", w, err)
			}
		}
	}

	// Some slack for fasthttp's worker pool, which shrinks on its own schedule.
	// What matters is that nothing scales with the number of connections.
	const tolerance = 20

	conns := waves * connsPerWave

	after := settleTo(t, baseline+tolerance, 60*time.Second)

	t.Logf("goroutines: %d before, %d after %d connections", baseline, after, conns)

	if after > baseline+tolerance {
		t.Errorf("goroutines went from %d to %d over %d connections, want them released",
			baseline, after, conns)
	}
}

// TestSoakLongLivedConnections holds connections open across many ping
// intervals with steady traffic. Anything retained per request or per ping
// shows up as heap that keeps climbing.
func TestSoakLongLivedConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("soak test skipped in short mode")
	}

	const (
		conns    = 25
		rounds   = 40
		perRound = 20
	)

	addr, _ := newAttackServer(t, ServerConfig{PingInterval: 50 * time.Millisecond})

	clients := make([]*fasthttp.HostClient, 0, conns)

	for i := 0; i < conns; i++ {
		hc := &fasthttp.HostClient{
			Addr:      addr,
			IsTLS:     true,
			TLSConfig: &tls.Config{InsecureSkipVerify: true},
		}

		if err := ConfigureClient(hc, ClientOpts{
			PingInterval:    50 * time.Millisecond,
			MaxResponseTime: 10 * time.Second,
		}); err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}

		clients = append(clients, hc)
	}

	t.Cleanup(func() {
		for _, hc := range clients {
			_ = ClientFrom(hc).Close()
		}
	})

	traffic := func() {
		for _, hc := range clients {
			for r := 0; r < perRound; r++ {
				req := fasthttp.AcquireRequest()
				res := fasthttp.AcquireResponse()

				req.SetRequestURI("https://" + addr + "/")
				req.Header.SetMethod(fasthttp.MethodGet)

				if err := hc.Do(req, res); err != nil {
					t.Errorf("request: %v", err)
				}

				fasthttp.ReleaseRequest(req)
				fasthttp.ReleaseResponse(res)
			}
		}
	}

	// Warm up first: pools and buffers fill on the first pass, and that growth
	// is not a leak.
	traffic()

	var early, late uint64

	grew := heapDelta(func() {
		for r := 0; r < rounds; r++ {
			traffic()

			// Give the ping timers room to fire between rounds.
			time.Sleep(20 * time.Millisecond)

			if r == rounds/4 {
				early = heapInUse()
			}
		}

		late = heapInUse()
	})

	requests := rounds * conns * perRound

	t.Logf("%d requests over %d long-lived connections: heap %.1f -> %.1f MiB, delta %.1f MiB (%.1f bytes per request)",
		requests, conns, float64(early)/(1<<20), float64(late)/(1<<20),
		float64(grew)/(1<<20), float64(grew)/float64(requests))

	if perRequest := float64(grew) / float64(requests); perRequest > 16 {
		t.Errorf("heap grows %.1f bytes per request on a held-open connection", perRequest)
	}
}

func heapInUse() uint64 {
	var m runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&m)

	return m.HeapAlloc
}
