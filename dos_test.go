package http2

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// The tests in this file drive abusive frame sequences at the server. They
// bypass Client and Conn entirely so a test can emit anything it likes,
// including sequences a well-behaved peer would never produce.

// hdr is a header field for the raw request builder below.
type hdr struct {
	key, value string
}

// attacker is a raw HTTP/2 client. Every method fails the test on I/O error
// except readFrame, which returns the error so a test can assert on how the
// connection ended.
type attacker struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK

	addr string
}

func dialAttacker(t *testing.T, addr string) *attacker {
	t.Helper()

	c, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{H2TLSProto},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	// Not AcquireHPACK: the shared pool is asserted on by TestAcquireHPACKAnd-
	// ReleaseHPACK, and borrowing from it here makes that test order dependent.
	enc := &HPACK{}
	enc.Reset()

	a := &attacker{
		t:    t,
		c:    c,
		br:   bufio.NewReader(c),
		bw:   bufio.NewWriter(c),
		enc:  enc,
		addr: addr,
	}

	st := &Settings{}
	st.Reset()

	if err := Handshake(true, a.bw, st, 1<<20); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	a.flush()

	return a
}

func (a *attacker) flush() {
	a.t.Helper()

	if err := a.bw.Flush(); err != nil && !isClosedConn(err) {
		a.t.Fatalf("flush: %v", err)
	}
}

// write serializes fr. It tolerates a closed connection, because a server that
// correctly rejects the attack closes it mid-write.
func (a *attacker) write(fr *FrameHeader) error {
	_, err := fr.WriteTo(a.bw)
	ReleaseFrameHeader(fr)

	return err
}

func (a *attacker) writeHeaders(id uint32, endStream, endHeaders bool, fields []hdr) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	for _, f := range fields {
		hf.Set(f.key, f.value)
		h.AppendHeaderField(a.enc, hf, false)
	}

	h.SetPadding(false)
	h.SetEndStream(endStream)
	h.SetEndHeaders(endHeaders)

	return a.write(fr)
}

// writeContinuation sends a CONTINUATION frame carrying count filler fields.
func (a *attacker) writeContinuation(id uint32, endHeaders bool, count int) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	cont := AcquireFrame(FrameContinuation).(*Continuation)
	fr.SetBody(cont)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	var block []byte

	for i := 0; i < count; i++ {
		hf.Set(fmt.Sprintf("x-filler-%d", i), "0123456789012345678901234567890123456789")
		block = a.enc.AppendHeader(block, hf, false)
	}

	cont.AppendHeader(block)

	cont.SetEndHeaders(endHeaders)

	return a.write(fr)
}

func (a *attacker) writeRST(id uint32, code ErrorCode) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	rst := AcquireFrame(FrameResetStream).(*RstStream)
	rst.SetCode(code)

	fr.SetBody(rst)

	return a.write(fr)
}

func (a *attacker) writePing() error {
	fr := AcquireFrameHeader()

	ping := AcquireFrame(FramePing).(*Ping)
	ping.SetCurrentTime()

	fr.SetBody(ping)

	return a.write(fr)
}

func (a *attacker) writeSettings() error {
	fr := AcquireFrameHeader()

	st := AcquireFrame(FrameSettings).(*Settings)
	st.Reset()

	fr.SetBody(st)

	return a.write(fr)
}

// readUntilGoAway drains frames until the server sends GOAWAY or the
// connection dies. It returns the GOAWAY code, or an error if the connection
// ended without one.
func (a *attacker) readUntilGoAway(timeout time.Duration) (ErrorCode, error) {
	_ = a.c.SetReadDeadline(time.Now().Add(timeout))

	for {
		fr, err := ReadFrameFrom(a.br)
		if err != nil {
			return 0, err
		}

		if ga, ok := fr.Body().(*GoAway); ok {
			code := ga.Code()
			ReleaseFrameHeader(fr)

			return code, nil
		}

		ReleaseFrameHeader(fr)
	}
}

func isClosedConn(err error) bool {
	return err != nil && (errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscallReset(err)))
}

// syscallReset keeps isClosedConn readable: a reset peer surfaces as an
// *net.OpError wrapping ECONNRESET or EPIPE, which errors.Is cannot match
// against a sentinel.
func syscallReset(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) {
		return err
	}

	return nil
}

// requestFields is a minimal valid request header block.
func requestFields(authority string) []hdr {
	return []hdr{
		{":method", "GET"},
		{":path", "/"},
		{":scheme", "https"},
		{":authority", authority},
	}
}

// newAttackServer starts a server and returns its address plus a counter of
// handler invocations, which measures how much work an attack extracted.
func newAttackServer(t *testing.T, cnf ServerConfig) (string, *atomic.Int64) {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	var handled atomic.Int64

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			handled.Add(1)
			ctx.SetBodyString("ok")
		},
		Logger: discardLogger{},
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

	return addr, &handled
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...interface{}) {}

// heapDelta reports how much the heap grew while fn ran, in bytes.
func heapDelta(fn func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	fn()

	runtime.GC()
	runtime.ReadMemStats(&after)

	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}

	return after.HeapAlloc - before.HeapAlloc
}

// drain reads frames in the background until the connection ends, recording the
// first GOAWAY code it sees. Without a drainer the server's writer channel
// fills, its write loop blocks on the socket, and the attack throttles itself
// instead of exercising the server.
type drainer struct {
	goaway    atomic.Int32
	gotGoAway atomic.Bool
	done      chan struct{}
}

func (a *attacker) drain() *drainer {
	d := &drainer{done: make(chan struct{})}

	go func() {
		defer close(d.done)

		for {
			fr, err := ReadFrameFrom(a.br)
			if err != nil {
				return
			}

			if ga, ok := fr.Body().(*GoAway); ok && !d.gotGoAway.Load() {
				d.goaway.Store(int32(ga.Code()))
				d.gotGoAway.Store(true)
			}

			ReleaseFrameHeader(fr)
		}
	}()

	return d
}

func (d *drainer) wait(timeout time.Duration) bool {
	select {
	case <-d.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// TestRapidReset drives CVE-2023-44487: open a stream and cancel it before it
// can become a request, over and over. The classic amplification is bypassing
// MAX_CONCURRENT_STREAMS to get unbounded work in flight at once. This server
// calls the handler inline from the single handleStreams goroutine, so requests
// are serialised per connection whatever the client does. What the test checks
// is that the churn does not accumulate state.
func TestRapidReset(t *testing.T) {
	const attempts = 20000

	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	fields := requestFields(addr)

	sent := 0

	grew := heapDelta(func() {
		for i := 0; i < attempts; i++ {
			id := uint32(i*2 + 1)

			// No END_STREAM: the request is never complete, so the only work
			// the server does is create the stream and tear it down again.
			if a.writeHeaders(id, false, true, fields) != nil {
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
		d.wait(2 * time.Second)
	})

	t.Logf("cancelled %d/%d streams, handler ran %d times, heap grew %.1f MiB, goaway=%v code=%s",
		sent, attempts, handled.Load(), float64(grew)/(1<<20),
		d.gotGoAway.Load(), ErrorCode(d.goaway.Load()))

	if n := handled.Load(); n != 0 {
		t.Errorf("handler ran %d times for streams that never completed a request", n)
	}

	if grew > 8<<20 {
		t.Errorf("heap grew %d bytes over %d cancelled streams, want the churn to leave nothing behind", grew, sent)
	}
}

// TestClosedStreamTableGrowth makes many ordinary requests on one connection.
// The server remembers every closed stream id so it can tell a late frame on a
// finished stream from a frame on an idle one, and that table must not grow
// without bound: a long-lived connection would carry it for its whole life.
func TestClosedStreamTableGrowth(t *testing.T) {
	requests := 40000
	if testing.Short() {
		requests = 4000
	}

	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	fields := requestFields(addr)

	sent := 0

	grew := heapDelta(func() {
		for i := 0; i < requests; i++ {
			if a.writeHeaders(uint32(i*2+1), true, true, fields) != nil {
				break
			}

			sent++

			if i%64 == 0 && a.bw.Flush() != nil {
				break
			}
		}

		_ = a.bw.Flush()

		// Let the server finish before sampling the heap.
		deadline := time.Now().Add(20 * time.Second)
		for handled.Load() < int64(sent) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	})

	_ = d

	perRequest := float64(grew) / float64(max(sent, 1))

	t.Logf("%d/%d requests, handler ran %d times, heap grew %.1f MiB (%.0f bytes per request)",
		sent, requests, handled.Load(), float64(grew)/(1<<20), perRequest)

	if sent != requests {
		t.Fatalf("only %d/%d requests went through", sent, requests)
	}

	// Anything that scales with the number of requests served is a leak: an
	// HTTP/2 connection is meant to be long lived.
	if perRequest > 8 {
		t.Errorf("server retains %.0f bytes per request, want it not to grow with the request count", perRequest)
	}
}

// TestContinuationFlood drives the 2024 CONTINUATION flood: a header block that
// never ends. Every frame adds header fields the server must decode and retain,
// on a stream that never becomes a request, so nothing bounds the work or the
// memory unless the server enforces a header list limit.
func TestContinuationFlood(t *testing.T) {
	const (
		frames         = 4000
		fieldsPerFrame = 100
	)

	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	sent := 0

	grew := heapDelta(func() {
		if a.writeHeaders(1, false, false, requestFields(addr)) != nil {
			return
		}

		for i := 0; i < frames; i++ {
			if a.writeContinuation(1, false, fieldsPerFrame) != nil {
				break
			}

			sent++

			if a.bw.Flush() != nil {
				break
			}
		}

		_ = a.bw.Flush()

		d.wait(10 * time.Second)
	})

	t.Logf("sent %d/%d continuation frames (%d header fields), heap grew %.1f MiB, handler ran %d, goaway=%v code=%s",
		sent, frames, sent*fieldsPerFrame, float64(grew)/(1<<20), handled.Load(),
		d.gotGoAway.Load(), ErrorCode(d.goaway.Load()))

	if !d.gotGoAway.Load() {
		t.Fatal("server accepted an endless header block without a GOAWAY")
	}

	if code := ErrorCode(d.goaway.Load()); code != EnhanceYourCalm {
		t.Errorf("goaway code = %s, want %s", code, EnhanceYourCalm)
	}

	if sent == frames {
		t.Error("server never cut the flood off")
	}

	if grew > 8<<20 {
		t.Errorf("heap grew %d bytes, want the header list limit to bound it", grew)
	}
}

// TestPingFlood and TestSettingsFlood check that a client which only sends
// control frames, and never reads the replies, cannot make the server buffer
// them without bound.
func TestPingFlood(t *testing.T) {
	testControlFlood(t, "ping", func(a *attacker) error { return a.writePing() })
}

func TestSettingsFlood(t *testing.T) {
	testControlFlood(t, "settings", func(a *attacker) error { return a.writeSettings() })
}

func testControlFlood(t *testing.T, name string, send func(*attacker) error) {
	t.Helper()

	const frames = 200000

	addr, _ := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)

	// Deliberately no drainer: every frame demands a reply the attacker never
	// reads, which is what makes this a flood rather than an echo test.
	sent := 0

	grew := heapDelta(func() {
		_ = a.c.SetWriteDeadline(time.Now().Add(5 * time.Second))

		for i := 0; i < frames; i++ {
			if send(a) != nil {
				break
			}

			sent++

			if i%256 == 0 && a.bw.Flush() != nil {
				break
			}
		}

		_ = a.bw.Flush()
	})

	t.Logf("%s: sent %d/%d, heap grew %.1f MiB", name, sent, frames, float64(grew)/(1<<20))

	// The server replies to every one of these and the attacker never reads,
	// so the replies must be bounded by backpressure rather than buffered.
	if grew > 8<<20 {
		t.Errorf("heap grew %d bytes over %d %s frames, want backpressure to bound it", grew, sent, name)
	}
}
