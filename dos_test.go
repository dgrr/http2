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

// writeContinuationFields sends a CONTINUATION frame carrying fields.
func (a *attacker) writeContinuationFields(id uint32, endHeaders bool, fields []hdr) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	cont := AcquireFrame(FrameContinuation).(*Continuation)
	fr.SetBody(cont)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	var block []byte

	for _, f := range fields {
		hf.Set(f.key, f.value)
		block = a.enc.AppendHeader(block, hf, false)
	}

	cont.AppendHeader(block)
	cont.SetEndHeaders(endHeaders)

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
	// completed counts the streams the server has finished with, so a test can
	// wait until the server is quiescent rather than until the last handler
	// returned. Handlers run off the stream loop, so those are not the same
	// moment: a returned handler still has a response to encode and send.
	completed atomic.Int64
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

			if fr.Stream() != 0 &&
				(fr.Flags().Has(FlagEndStream) || fr.Type() == FrameResetStream) {
				d.completed.Add(1)
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
// can become a request, over and over. Nothing here ever completes a request,
// so no handler runs and what the test checks is that the churn accumulates no
// state. TestRapidResetBoundsHandlers covers the amplification itself, where
// the requests do complete and each cancellation tries to buy another handler.
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

	t.Logf("canceled %d/%d streams, handler ran %d times, heap grew %.1f MiB, goaway=%v code=%s",
		sent, attempts, handled.Load(), float64(grew)/(1<<20),
		d.gotGoAway.Load(), ErrorCode(d.goaway.Load()))

	if n := handled.Load(); n != 0 {
		t.Errorf("handler ran %d times for streams that never completed a request", n)
	}

	if grew > 8<<20 {
		t.Errorf("heap grew %d bytes over %d canceled streams, want the churn to leave nothing behind", grew, sent)
	}
}

// requestChurn drives requests down one fresh connection and reports how much
// heap the server kept once it had answered all of them.
//
// Concurrency is pinned low on purpose. A server that handles many streams at
// once holds a stream and a RequestCtx for each one, and the pools settle at
// that high-water mark, which is a fixed cost that would otherwise be read as
// growth.
func requestChurn(t *testing.T, requests int) uint64 {
	t.Helper()

	addr, _ := newAttackServer(t, ServerConfig{
		PingInterval:         -1,
		MaxConcurrentStreams: 16,
	})

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

		// Wait for the responses, not for the handlers: a handler that has
		// returned still has a stream attached to it until the stream loop has
		// encoded and queued its response.
		deadline := time.Now().Add(30 * time.Second)
		for d.completed.Load() < int64(sent) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		// Nothing is in flight now, so the pools have handed everything back
		// and what is left is what the connection is keeping.
		time.Sleep(100 * time.Millisecond)
	})

	if sent != requests {
		t.Fatalf("only %d/%d requests went through", sent, requests)
	}

	if got := d.completed.Load(); got < int64(sent) {
		t.Fatalf("only %d/%d requests were answered", got, sent)
	}

	return grew
}

// TestClosedStreamTableGrowth makes many ordinary requests on one connection.
// The server remembers every closed stream id so it can tell a late frame on a
// finished stream from a frame on an idle one, and that table must not grow
// without bound: a long-lived connection would carry it for its whole life.
//
// It compares two runs an order of magnitude apart rather than dividing one run
// by its request count. Serving a connection has a fixed cost that has nothing
// to do with how many requests went down it, and at small counts that fixed
// cost swamps a per-request average. What a table that grows per request looks
// like is the second run costing ten times the first.
func TestClosedStreamTableGrowth(t *testing.T) {
	small, large := 4000, 40000
	if testing.Short() {
		small, large = 1000, 10000
	}

	// Warm the pools first, so the measured runs are a steady state rather than
	// the cost of starting up.
	requestChurn(t, small)

	base := requestChurn(t, small)
	grew := requestChurn(t, large)

	t.Logf("%d requests kept %.2f MiB, %d requests kept %.2f MiB",
		small, float64(base)/(1<<20), large, float64(grew)/(1<<20))

	// Ten times the requests for four times the memory is already generous:
	// anything that scales per request would be at ten.
	if limit := base * 4; grew > limit {
		t.Errorf("%dx the requests kept %d bytes against %d for %dx, want the "+
			"cost not to scale with the request count",
			large/small, grew, base, small)
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
