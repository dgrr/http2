package http2

import (
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// idxHdr is a header field plus whether the encoder should add it to its
// dynamic table, so a test can control exactly what the peer's table holds.
type idxHdr struct {
	key, value string
	index      bool
}

// writeIndexedHeaders sends a HEADERS frame, indexing each field as the test
// asks rather than never indexing as writeHeaders does.
func (a *attacker) writeIndexedHeaders(id uint32, endStream, endHeaders bool, fields []idxHdr) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	for _, f := range fields {
		hf.Set(f.key, f.value)
		h.AppendHeaderField(a.enc, hf, f.index)
	}

	h.SetPadding(false)
	h.SetEndStream(endStream)
	h.SetEndHeaders(endHeaders)

	return a.write(fr)
}

// newCaptureServer starts a server that reports the value of one header field
// for every request it handles.
func newCaptureServer(t *testing.T, key string, cnf ServerConfig) (string, chan string) {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	got := make(chan string, 8)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			select {
			case got <- string(ctx.Request.Header.Peek(key)):
			default:
			}

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

	return addr, got
}

// TestServerKeepsHPACKInSyncAfterStreamError covers RFC 7541 4.1 and RFC 7540
// 4.3: HPACK is a running conversation between the peer's encoder and ours, so
// every field of every block has to go through the decoder even when the
// request it belongs to is being rejected. Abandoning a block halfway on a
// stream error leaves our dynamic table one entry short of the peer's for the
// life of the connection, and from then on an indexed field decodes as
// whatever sits at that index in our table instead. Behind a reverse proxy
// that multiplexes clients onto one upstream connection, that is one client's
// headers turning up on another client's request.
func TestServerKeepsHPACKInSyncAfterStreamError(t *testing.T) {
	addr, got := newCaptureServer(t, "x-user", ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	// The rejected block. x-user: victim is indexed by both sides, the
	// uppercase field is not indexed by either, and x-user: attacker is
	// indexed by the peer alone if the server stops decoding at the uppercase
	// field. That leaves the two tables one entry out of step.
	if err := a.writeIndexedHeaders(1, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "victim", true},
		{"X-Uppercase", "1", false},
		{"x-user", "attacker", true},
	}); err != nil {
		t.Fatalf("writing the rejected request: %v", err)
	}

	a.flush()

	// The next request references x-user: attacker by index. A decoder in step
	// with the peer resolves it to exactly that.
	if err := a.writeIndexedHeaders(3, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "attacker", false},
	}); err != nil {
		t.Fatalf("writing the second request: %v", err)
	}

	a.flush()

	select {
	case v := <-got:
		if v != "attacker" {
			t.Errorf("handler saw x-user = %q, want %q: the decoder is out of step with the peer",
				v, "attacker")
		}
	case <-d.done:
		t.Errorf("server closed the connection instead of serving the second request (goaway %s)",
			ErrorCode(d.goaway.Load()))
	case <-time.After(5 * time.Second):
		t.Error("the second request never reached the handler")
	}
}

// writeIndexedHeaderBlock sends a response header block, indexing each field as
// the test asks rather than never indexing as writeHeaderBlock does.
func (p *peer) writeIndexedHeaderBlock(id uint32, endStream bool, fields []idxHdr) {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	for _, f := range fields {
		hf.Set(f.key, f.value)
		h.AppendHeaderField(p.enc, hf, f.index)
	}

	h.SetPadding(false)
	h.SetEndHeaders(true)
	h.SetEndStream(endStream)

	_, _ = fr.WriteTo(p.bw)

	ReleaseFrameHeader(fr)

	_ = p.bw.Flush()
}

// headerWithin runs one request and returns the value the response carried for
// key, so a test can assert on what the decoder made of the block.
func headerWithin(t *testing.T, hc *fasthttp.HostClient, addr, key string, d time.Duration) (string, error) {
	t.Helper()

	type outcome struct {
		value string
		err   error
	}

	done := make(chan outcome, 1)

	go func() {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodGet)

		err := hc.Do(req, res)

		done <- outcome{value: string(res.Header.Peek(key)), err: err}
	}()

	select {
	case o := <-done:
		return o.value, o.err
	case <-time.After(d):
		t.Fatalf("request did not return within %s", d)
		return "", nil
	}
}

// TestClientKeepsHPACKInSyncAfterRejectedResponse is
// TestServerKeepsHPACKInSyncAfterStreamError from the other end. The client
// rejects a malformed response and keeps the connection, so abandoning the
// block halfway leaves its decoder short of the server's encoder and every
// later response on that connection decodes against the wrong table.
func TestClientKeepsHPACKInSyncAfterRejectedResponse(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()
		if id == 0 {
			return
		}

		p.writeIndexedHeaderBlock(id, true, []idxHdr{
			{":status", "200", false},
			{"x-user", "victim", true},
			{"X-Uppercase", "1", false},
			{"x-user", "attacker", true},
		})

		id = p.waitForRequest()
		if id == 0 {
			return
		}

		p.writeIndexedHeaderBlock(id, true, []idxHdr{
			{":status", "200", false},
			{"x-user", "attacker", false},
		})
	})

	hc := clientFor(t, addr)

	if _, err := doWithin(t, hc, addr, 10*time.Second); err == nil {
		t.Fatal("request reported success on a response with an uppercase field name")
	}

	v, err := headerWithin(t, hc, addr, "x-user", 10*time.Second)
	if err != nil {
		t.Fatalf("second request on the same connection: %v", err)
	}

	if v != "attacker" {
		t.Errorf("response carried x-user = %q, want %q: the decoder is out of step with the peer",
			v, "attacker")
	}
}

// writeIndexedContinuation sends a CONTINUATION frame, indexing each field as
// the test asks.
func (a *attacker) writeIndexedContinuation(id uint32, endHeaders bool, fields []idxHdr) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	cont := AcquireFrame(FrameContinuation).(*Continuation)
	fr.SetBody(cont)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	var block []byte

	for _, f := range fields {
		hf.Set(f.key, f.value)
		block = a.enc.AppendHeader(block, hf, f.index)
	}

	cont.AppendHeader(block)
	cont.SetEndHeaders(endHeaders)

	return a.write(fr)
}

// TestServerKeepsHPACKInSyncAcrossContinuation is
// TestServerKeepsHPACKInSyncAfterStreamError with the block split over a
// CONTINUATION frame. The field that rejects the request is in the first frame
// and the field that has to reach the decoder anyway is in the second, so the
// stream error cannot be raised until END_HEADERS without losing the rest of
// the block.
func TestServerKeepsHPACKInSyncAcrossContinuation(t *testing.T) {
	addr, got := newCaptureServer(t, "x-user", ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	if err := a.writeIndexedHeaders(1, true, false, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "victim", true},
		{"X-Uppercase", "1", false},
	}); err != nil {
		t.Fatalf("writing the rejected request: %v", err)
	}

	a.flush()

	if err := a.writeIndexedContinuation(1, true, []idxHdr{
		{"x-user", "attacker", true},
	}); err != nil {
		t.Fatalf("writing the continuation: %v", err)
	}

	a.flush()

	if err := a.writeIndexedHeaders(3, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "attacker", false},
	}); err != nil {
		t.Fatalf("writing the second request: %v", err)
	}

	a.flush()

	select {
	case v := <-got:
		if v != "attacker" {
			t.Errorf("handler saw x-user = %q, want %q: the decoder is out of step with the peer",
				v, "attacker")
		}
	case <-d.done:
		t.Errorf("server closed the connection instead of serving the second request (goaway %s)",
			ErrorCode(d.goaway.Load()))
	case <-time.After(5 * time.Second):
		t.Error("the second request never reached the handler")
	}
}

// TestServerKeepsHPACKInSyncOnRefusedStream covers RFC 7540 5.1.1 and 8.1: a
// stream refused over SETTINGS_MAX_CONCURRENT_STREAMS still arrived with a
// header block, and the peer's encoder has indexed every field in it. Dropping
// the block instead of decoding it leaves the decoder out of step for the life
// of the connection. A client that races the limit does this without meaning
// to, so this needs no attacker at all.
func TestServerKeepsHPACKInSyncOnRefusedStream(t *testing.T) {
	addr, got := newCaptureServer(t, "x-user", ServerConfig{
		PingInterval:         -1,
		MaxConcurrentStreams: 1,
	})

	a := dialAttacker(t, addr)
	d := a.drain()

	// Stream 1 stays open: no END_STREAM, so it holds the only slot.
	if err := a.writeIndexedHeaders(1, false, true, []idxHdr{
		{":method", "POST", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
	}); err != nil {
		t.Fatalf("writing the first request: %v", err)
	}

	a.flush()

	// Stream 3 is refused, but its block still indexes a field.
	if err := a.writeIndexedHeaders(3, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "attacker", true},
	}); err != nil {
		t.Fatalf("writing the refused request: %v", err)
	}

	a.flush()

	// Let stream 1 finish so a slot frees up.
	if err := a.writeData(1, true, nil); err != nil {
		t.Fatalf("ending the first request: %v", err)
	}

	a.flush()

	// Drop the value the first request produced.
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the handler")
	}

	if err := a.writeIndexedHeaders(5, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "attacker", false},
	}); err != nil {
		t.Fatalf("writing the third request: %v", err)
	}

	a.flush()

	select {
	case v := <-got:
		if v != "attacker" {
			t.Errorf("handler saw x-user = %q, want %q: the decoder is out of step with the peer",
				v, "attacker")
		}
	case <-d.done:
		t.Errorf("server closed the connection instead of serving the request (goaway %s)",
			ErrorCode(d.goaway.Load()))
	case <-time.After(5 * time.Second):
		t.Error("the request never reached the handler")
	}
}

// TestServerKeepsHPACKInSyncOnRefusedContinuation is the refused-stream case
// with the block split, so the drain has to carry a field the frame boundary
// cut in half across to the CONTINUATION.
func TestServerKeepsHPACKInSyncOnRefusedContinuation(t *testing.T) {
	addr, got := newCaptureServer(t, "x-user", ServerConfig{
		PingInterval:         -1,
		MaxConcurrentStreams: 1,
	})

	a := dialAttacker(t, addr)
	d := a.drain()

	if err := a.writeIndexedHeaders(1, false, true, []idxHdr{
		{":method", "POST", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
	}); err != nil {
		t.Fatalf("writing the first request: %v", err)
	}

	a.flush()

	if err := a.writeIndexedHeaders(3, false, false, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
	}); err != nil {
		t.Fatalf("writing the refused request: %v", err)
	}

	a.flush()

	if err := a.writeIndexedContinuation(3, true, []idxHdr{
		{"x-user", "attacker", true},
	}); err != nil {
		t.Fatalf("writing the refused continuation: %v", err)
	}

	a.flush()

	if err := a.writeData(1, true, nil); err != nil {
		t.Fatalf("ending the first request: %v", err)
	}

	a.flush()

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the first request never reached the handler")
	}

	if err := a.writeIndexedHeaders(5, true, true, []idxHdr{
		{":method", "GET", false},
		{":scheme", "https", false},
		{":path", "/", false},
		{":authority", addr, false},
		{"x-user", "attacker", false},
	}); err != nil {
		t.Fatalf("writing the third request: %v", err)
	}

	a.flush()

	select {
	case v := <-got:
		if v != "attacker" {
			t.Errorf("handler saw x-user = %q, want %q: the decoder is out of step with the peer",
				v, "attacker")
		}
	case <-d.done:
		t.Errorf("server closed the connection instead of serving the request (goaway %s)",
			ErrorCode(d.goaway.Load()))
	case <-time.After(5 * time.Second):
		t.Error("the request never reached the handler")
	}
}
