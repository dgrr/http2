package http2

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// h2spec only drives servers, so the client has no spec suite. These tests are
// the equivalent from the other side: a raw server that emits frame sequences a
// well-behaved peer never would, checking the one property a client owes its
// caller under all of them, which is that every request finishes. A client that
// hangs or panics on hostile input is worse than one that returns an error.

// peer is the raw server side of a connection under test.
type peer struct {
	t  *testing.T
	c  net.Conn
	br *bufio.Reader
	bw *bufio.Writer

	enc *HPACK
}

// newRawServer starts a TLS listener that hands each accepted connection to
// handle after the HTTP/2 preface and the SETTINGS exchange.
func newRawServer(t *testing.T, handle func(p *peer)) string {
	return newRawServerSettings(t, nil, handle)
}

// newRawServerSettings is newRawServer with a say in the SETTINGS the peer
// opens with, which is how a test pins the client's send windows.
func newRawServerSettings(t *testing.T, tune func(*Settings), handle func(p *peer)) string {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{H2TLSProto},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer func() { _ = c.Close() }()

				// A panic in here is the test's problem, not the client's, but
				// letting it escape would take the whole run down.
				defer func() { _ = recover() }()

				enc := &HPACK{}
				enc.Reset()

				p := &peer{
					t:   t,
					c:   c,
					br:  bufio.NewReader(c),
					bw:  bufio.NewWriter(c),
					enc: enc,
				}

				if !ReadPreface(p.br) {
					return
				}

				p.writeSettings(tune)

				handle(p)
			}()
		}
	}()

	return ln.Addr().String()
}

func (p *peer) writeSettings(tune func(*Settings)) {
	fr := AcquireFrameHeader()

	st := AcquireFrame(FrameSettings).(*Settings)
	st.Reset()

	if tune != nil {
		tune(st)
	}

	fr.SetBody(st)

	_, _ = fr.WriteTo(p.bw)

	ReleaseFrameHeader(fr)

	_ = p.bw.Flush()
}

// readFrame returns the next frame, or nil once the connection ends.
func (p *peer) readFrame() *FrameHeader {
	_ = p.c.SetReadDeadline(time.Now().Add(10 * time.Second))

	fr, err := ReadFrameFrom(p.br)
	if err != nil {
		return nil
	}

	return fr
}

// waitForRequest drains until a HEADERS frame arrives and returns its stream id.
func (p *peer) waitForRequest() uint32 {
	for {
		fr := p.readFrame()
		if fr == nil {
			return 0
		}

		id := fr.Stream()
		isHeaders := fr.Type() == FrameHeaders

		ReleaseFrameHeader(fr)

		if isHeaders {
			return id
		}
	}
}

// writeResponse sends a minimal complete response on the stream.
func (p *peer) writeResponse(id uint32, status string) {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	hf.Set(":status", status)
	h.AppendHeaderField(p.enc, hf, false)

	hf.Set("content-length", "2")
	h.AppendHeaderField(p.enc, hf, false)

	h.SetPadding(false)
	h.SetEndHeaders(true)
	h.SetEndStream(false)

	_, _ = fr.WriteTo(p.bw)
	ReleaseFrameHeader(fr)

	d := AcquireFrameHeader()
	d.SetStream(id)

	data := AcquireFrame(FrameData).(*Data)
	data.SetData([]byte("ok"))
	data.SetEndStream(true)

	d.SetBody(data)

	_, _ = d.WriteTo(p.bw)
	ReleaseFrameHeader(d)

	_ = p.bw.Flush()
}

// writeRaw emits a frame byte for byte, so a test can send frame types and
// payloads the library's own types cannot express.
func (p *peer) writeRaw(kind, flags byte, stream uint32, payload []byte) {
	var header [9]byte

	header[0] = byte(len(payload) >> 16)
	header[1] = byte(len(payload) >> 8)
	header[2] = byte(len(payload))
	header[3] = kind
	header[4] = flags

	binary.BigEndian.PutUint32(header[5:], stream)

	_, _ = p.bw.Write(header[:])
	_, _ = p.bw.Write(payload)
	_ = p.bw.Flush()
}

func (p *peer) writeRST(id uint32, code ErrorCode) {
	var payload [4]byte

	binary.BigEndian.PutUint32(payload[:], uint32(code))

	p.writeRaw(byte(FrameResetStream), 0, id, payload[:])
}

func (p *peer) writeWindowUpdate(id, inc uint32) {
	var payload [4]byte

	binary.BigEndian.PutUint32(payload[:], inc)

	p.writeRaw(byte(FrameWindowUpdate), 0, id, payload[:])
}

func (p *peer) writeGoAway(lastID uint32, code ErrorCode) {
	var payload [8]byte

	binary.BigEndian.PutUint32(payload[:4], lastID)
	binary.BigEndian.PutUint32(payload[4:], uint32(code))

	p.writeRaw(byte(FrameGoAway), 0, 0, payload[:])
}

// clientFor points a configured client at addr. It returns nil when the
// handshake itself is what the test expects to fail.
func clientFor(t *testing.T, addr string) *fasthttp.HostClient {
	t.Helper()

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{
		PingInterval:    -1,
		MaxResponseTime: 3 * time.Second,
	}); err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}

	t.Cleanup(func() { _ = ClientFrom(hc).Close() })

	return hc
}

// doWithin runs one request and fails the test if it has not returned by the
// deadline, which is what turns a hang into a readable failure.
func doWithin(t *testing.T, hc *fasthttp.HostClient, addr string, d time.Duration) (int, error) {
	t.Helper()

	type outcome struct {
		status int
		err    error
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

		done <- outcome{status: res.StatusCode(), err: err}
	}()

	select {
	case o := <-done:
		return o.status, o.err
	case <-time.After(d):
		t.Fatalf("request did not return within %s", d)
		return 0, nil
	}
}

func TestClientAgainstGoAway(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		p.waitForRequest()
		p.writeGoAway(0, NoError)
	})

	hc := clientFor(t, addr)

	if _, err := doWithin(t, hc, addr, 10*time.Second); err == nil {
		t.Error("request reported success after the server sent GOAWAY")
	}
}

func TestClientAgainstRstStream(t *testing.T) {
	// The connection stays up after the reset. Closing it would end the request
	// too, so the test would pass without the client ever reading the frame.
	release := make(chan struct{})

	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()
		p.writeRST(id, StreamCanceled)

		<-release
	})

	t.Cleanup(func() { close(release) })

	hc := clientFor(t, addr)

	start := time.Now()

	_, err := doWithin(t, hc, addr, 10*time.Second)
	if err == nil {
		t.Fatal("request reported success after the server reset the stream")
	}

	// clientFor allows 3s before it cancels the stream itself. Reacting to the
	// frame is immediate; falling back to the timeout is the bug.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("RST_STREAM took %s to surface, so it timed out instead", elapsed)
	}

	if !errors.Is(err, StreamCanceled) {
		t.Errorf("err = %v, want the code the server sent", err)
	}
}

// TestClientIgnoresUnknownFrameType covers RFC 7540 4.1: a frame of a type the
// endpoint does not know must be discarded, not treated as an error.
func TestClientIgnoresUnknownFrameType(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()

		p.writeRaw(0x1f, 0, 0, []byte("extension payload"))
		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	status, err := doWithin(t, hc, addr, 10*time.Second)
	if err != nil {
		t.Fatalf("unknown frame type broke the request: %v", err)
	}

	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
}

// TestClientIgnoresUnknownSetting covers RFC 7540 6.5.2: an unknown setting
// identifier must be ignored.
func TestClientIgnoresUnknownSetting(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()

		var payload [6]byte

		binary.BigEndian.PutUint16(payload[:2], 0xfeff)
		binary.BigEndian.PutUint32(payload[2:], 1)

		p.writeRaw(byte(FrameSettings), 0, 0, payload[:])
		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	status, err := doWithin(t, hc, addr, 10*time.Second)
	if err != nil {
		t.Fatalf("unknown setting broke the request: %v", err)
	}

	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
}

// TestClientAgainstSilentServer is the hang check: a server that accepts the
// stream and then says nothing must not park the caller forever.
func TestClientAgainstSilentServer(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		p.waitForRequest()

		// Hold the connection open without answering.
		time.Sleep(20 * time.Second)
	})

	hc := clientFor(t, addr)

	start := time.Now()

	_, err := doWithin(t, hc, addr, 15*time.Second)
	if err == nil {
		t.Fatal("request reported success from a server that never replied")
	}

	// MaxResponseTime is 3s, so anything near the 15s ceiling means the
	// cancellation did not work.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("request took %s to give up, want about MaxResponseTime", elapsed)
	}
}

// TestClientAgainstMalformedHeaderBlock feeds the response decoder bytes that
// are not a valid header block.
func TestClientAgainstMalformedHeaderBlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block []byte
	}{
		{name: "truncated varint", block: []byte{0xff, 0xff, 0xff, 0xff, 0xff}},
		{name: "index past the table", block: []byte{0xff, 0xd7, 0xd7, 0xd7, 0xd7, 0xd7, 0xd7, 0x7f}},
		{name: "truncated literal", block: []byte{0x41}},
		{name: "empty", block: []byte{}},
		{name: "random", block: []byte{0x00, 0x88, 0x99, 0xaa, 0xbb, 0xcc}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := newRawServer(t, func(p *peer) {
				id := p.waitForRequest()

				// END_HEADERS | END_STREAM
				p.writeRaw(byte(FrameHeaders), 0x4|0x1, id, tc.block)
			})

			hc := clientFor(t, addr)

			// The only requirement is that it comes back at all: a panic in the
			// read loop or a stream that never resolves is the failure.
			_, _ = doWithin(t, hc, addr, 10*time.Second)
		})
	}
}

// TestClientAgainstOversizedFrame sends a frame longer than the default
// SETTINGS_MAX_FRAME_SIZE the client advertised.
func TestClientAgainstOversizedFrame(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()

		p.writeRaw(byte(FrameData), 0, id, make([]byte, (1<<14)+1))
	})

	hc := clientFor(t, addr)

	_, _ = doWithin(t, hc, addr, 10*time.Second)
}

// TestClientAgainstFrameOnIdleStream sends frames on stream ids the client
// never opened.
func TestClientAgainstFrameOnIdleStream(t *testing.T) {
	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()

		p.writeRaw(byte(FrameData), 0, 777, []byte("not yours"))
		p.writeRST(999, StreamCanceled)
		p.writeResponse(id, "200")
	})

	hc := clientFor(t, addr)

	_, _ = doWithin(t, hc, addr, 10*time.Second)
}

// writeHeaderBlock sends a HEADERS frame carrying exactly the fields given, so
// a test can put a response together that the encoder side would never build.
func (p *peer) writeHeaderBlock(id uint32, endStream bool, fields []hdr) {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	h := AcquireFrame(FrameHeaders).(*Headers)
	fr.SetBody(h)

	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	for _, f := range fields {
		hf.Set(f.key, f.value)
		h.AppendHeaderField(p.enc, hf, false)
	}

	h.SetPadding(false)
	h.SetEndHeaders(true)
	h.SetEndStream(endStream)

	_, _ = fr.WriteTo(p.bw)

	ReleaseFrameHeader(fr)

	_ = p.bw.Flush()
}

// TestClientRejectsMalformedResponseHeaders covers RFC 7540 8.1.2.x from the
// client's side. Each of these used to be either accepted silently or, for the
// one byte pseudo-header name, a panic on the read loop.
func TestClientRejectsMalformedResponseHeaders(t *testing.T) {
	cases := []struct {
		name   string
		fields []hdr
	}{
		{
			// The name is shorter than the offset the status check indexed.
			name:   "one byte pseudo-header name",
			fields: []hdr{{":", "200"}},
		},
		{
			name:   "request pseudo-header in a response",
			fields: []hdr{{":status", "200"}, {":method", "GET"}},
		},
		{
			name:   "pseudo-header after a regular field",
			fields: []hdr{{"x-thing", "1"}, {":status", "200"}},
		},
		{
			name:   "status that is not a number",
			fields: []hdr{{":status", "abc"}},
		},
		{
			name:   "status outside the three digit range",
			fields: []hdr{{":status", "7"}},
		},
		{
			name:   "uppercase field name",
			fields: []hdr{{":status", "200"}, {"X-Thing", "1"}},
		},
		{
			name:   "connection-specific field",
			fields: []hdr{{":status", "200"}, {"connection", "keep-alive"}},
		},
		{
			name:   "content-length that is not a number",
			fields: []hdr{{":status", "200"}, {"content-length", "many"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})

			addr := newRawServer(t, func(p *peer) {
				id := p.waitForRequest()
				if id == 0 {
					return
				}

				p.writeHeaderBlock(id, true, tc.fields)

				<-release
			})

			t.Cleanup(func() { close(release) })

			hc := clientFor(t, addr)

			start := time.Now()

			if _, err := doWithin(t, hc, addr, 10*time.Second); err == nil {
				t.Error("request reported success on a malformed response")
			}

			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("took %s to fail, so it timed out rather than rejecting the headers", elapsed)
			}
		})
	}
}

// TestClientRejectsUnsolicitedPush covers RFC 7540 6.5.2: the client
// advertises SETTINGS_ENABLE_PUSH of 0, so a PUSH_PROMISE from the server is a
// connection error. Ignoring the frame left the server holding a stream that
// would never be read.
func TestClientRejectsUnsolicitedPush(t *testing.T) {
	release := make(chan struct{})

	addr := newRawServer(t, func(p *peer) {
		id := p.waitForRequest()
		if id == 0 {
			return
		}

		// A PUSH_PROMISE on the request's stream, promising stream 2.
		var payload [4]byte

		binary.BigEndian.PutUint32(payload[:], 2)

		p.writeRaw(byte(FramePushPromise), byte(FlagEndHeaders), id, payload[:])

		<-release
	})

	t.Cleanup(func() { close(release) })

	hc := clientFor(t, addr)

	start := time.Now()

	if _, err := doWithin(t, hc, addr, 10*time.Second); err == nil {
		t.Error("request reported success after an unsolicited push")
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s to fail, so the push was ignored rather than rejected", elapsed)
	}
}

// TestClientAdvertisesPushDisabled covers RFC 7540 6.5.2: SETTINGS_ENABLE_PUSH
// defaults to 1, so a client that will not accept pushed streams has to send
// the 0 itself. This client answers a PUSH_PROMISE by tearing the connection
// down, which is only defensible if it said so first.
func TestClientAdvertisesPushDisabled(t *testing.T) {
	got := make(chan map[uint16]uint32, 1)

	addr := newRawServer(t, func(p *peer) {
		for {
			fr := p.readFrame()
			if fr == nil {
				return
			}

			if fr.Type() == FrameSettings && !fr.Body().(*Settings).IsAck() {
				select {
				case got <- decodeSettings(fr.payload):
				default:
				}
			}

			ReleaseFrameHeader(fr)
		}
	})

	clientFor(t, addr)

	select {
	case st := <-got:
		v, ok := st[EnablePush]
		if !ok {
			t.Fatalf("client settings %v carry no SETTINGS_ENABLE_PUSH", st)
		}

		if v != 0 {
			t.Errorf("SETTINGS_ENABLE_PUSH = %d, want 0", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the client never sent a SETTINGS frame")
	}
}
