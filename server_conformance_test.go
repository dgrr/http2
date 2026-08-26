package http2

import (
	"bufio"
	"crypto/tls"
	"encoding/binary"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// h2spec covers the server against the spec text. These fill in the cases it
// does not reach, using the raw client from dos_test.go.

// writeRaw emits a frame byte for byte, so a test can send frame types the
// library's own types cannot express.
func (a *attacker) writeRaw(kind, flags byte, stream uint32, payload []byte) error {
	var header [9]byte

	header[0] = byte(len(payload) >> 16)
	header[1] = byte(len(payload) >> 8)
	header[2] = byte(len(payload))
	header[3] = kind
	header[4] = flags

	binary.BigEndian.PutUint32(header[5:], stream)

	if _, err := a.bw.Write(header[:]); err != nil {
		return err
	}

	if _, err := a.bw.Write(payload); err != nil {
		return err
	}

	return a.bw.Flush()
}

// writeData sends a DATA frame on a stream.
func (a *attacker) writeData(id uint32, endStream bool, body []byte) error {
	fr := AcquireFrameHeader()
	fr.SetStream(id)

	data := AcquireFrame(FrameData).(*Data)
	data.SetPadding(false)
	data.SetEndStream(endStream)
	data.SetData(body)

	fr.SetBody(data)

	return a.write(fr)
}

// TestServerIgnoresUnknownFrameType covers RFC 7540 4.1 and 5.5: a frame of an
// unknown type is discarded. Extension frames such as ALTSVC, ORIGIN and
// PRIORITY_UPDATE are the ordinary reason a peer sends one, so rejecting them
// breaks connections that are behaving correctly.
func TestServerIgnoresUnknownFrameType(t *testing.T) {
	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	if err := a.writeRaw(0x1f, 0, 0, []byte("extension payload")); err != nil {
		t.Fatalf("writing the extension frame: %v", err)
	}

	if err := a.writeHeaders(1, true, true, requestFields(addr)); err != nil {
		t.Fatalf("writing the request: %v", err)
	}

	a.flush()

	deadline := time.Now().Add(5 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if handled.Load() == 0 {
		t.Error("the request never reached the handler after an extension frame")
	}

	if d.gotGoAway.Load() {
		t.Errorf("server sent GOAWAY %s for an unknown frame type, want it discarded",
			ErrorCode(d.goaway.Load()))
	}
}

// TestServerRejectsExtensionFrameInHeaderBlock covers the exception in RFC 7540
// 6.10: nothing may be interleaved between a HEADERS frame and the
// CONTINUATION frames that complete it, extension frames included.
func TestServerRejectsExtensionFrameInHeaderBlock(t *testing.T) {
	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	// HEADERS without END_HEADERS opens a header block.
	if err := a.writeHeaders(1, false, false, requestFields(addr)); err != nil {
		t.Fatalf("writing the request: %v", err)
	}

	a.flush()

	if err := a.writeRaw(0x1f, 0, 0, []byte("extension payload")); err != nil {
		t.Fatalf("writing the extension frame: %v", err)
	}

	if !d.wait(5 * time.Second) {
		t.Error("server kept the connection open after an extension frame inside a header block")
	}

	if !d.gotGoAway.Load() {
		t.Error("no GOAWAY for an extension frame inside a header block")
	} else if code := ErrorCode(d.goaway.Load()); code != ProtocolError {
		t.Errorf("goaway code = %s, want %s", code, ProtocolError)
	}

	if n := handled.Load(); n != 0 {
		t.Errorf("handler ran %d times for a header block that was never completed", n)
	}
}

// TestServerAcceptsTableSizeUpdateInTrailers covers RFC 7541 4.2: a dynamic
// table size update belongs at the start of the first header block after the
// size changed, which is not necessarily the first block on the stream. The
// server used to accept one only in a stream's opening block, so a peer whose
// SETTINGS arrived while its request headers were already in flight had its
// connection killed with COMPRESSION_ERROR when it put the update in the
// trailers. net/http's client does exactly that, intermittently.
func TestServerAcceptsTableSizeUpdateInTrailers(t *testing.T) {
	addr, handled := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	// Request headers, body, then a trailer block that opens with a dynamic
	// table size update.
	if err := a.writeHeaders(1, false, true, requestFields(addr)); err != nil {
		t.Fatalf("writing the request: %v", err)
	}

	a.flush()

	var block []byte

	// 001xxxxx with a 5-bit integer: set the table size to 4096.
	block = appendInt(append(block, 0x20), 5, 4096)

	hf := AcquireHeaderField()
	hf.Set("x-checksum", "abc123")

	block = a.enc.AppendHeader(block, hf, false)

	ReleaseHeaderField(hf)

	if err := a.writeRaw(byte(FrameHeaders), byte(FlagEndStream|FlagEndHeaders), 1, block); err != nil {
		t.Fatalf("writing the trailers: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if handled.Load() == 0 {
		t.Error("the request never reached the handler")
	}

	if d.gotGoAway.Load() {
		t.Errorf("server sent GOAWAY %s for a table size update in the trailers",
			ErrorCode(d.goaway.Load()))
	}
}

// TestServerDoesNotCancelAnOpenRequest covers a request that is still being
// sent when the connection's request timer first fires. The timer was created
// already expired, and with no read timeout configured every open stream looked
// overdue, so a request whose body had not finished arriving was answered with
// RST_STREAM(CANCEL) and nothing in the logs to explain it.
//
// Any request with a body is exposed: the stream stays open between the headers
// and the last DATA frame, which is exactly the window the stale tick lands in.
func TestServerDoesNotCancelAnOpenRequest(t *testing.T) {
	const conns = 20

	addr, _ := newAttackServer(t, ServerConfig{PingInterval: -1})

	for i := 0; i < conns; i++ {
		func() {
			a := dialAttacker(t, addr)

			defer func() { _ = a.c.Close() }()

			// Headers without END_STREAM: the stream is open and waiting for a
			// body from here on.
			if err := a.writeHeaders(1, false, true, requestFields(addr)); err != nil {
				t.Fatalf("connection %d: request: %v", i, err)
			}

			a.flush()

			// Long enough for the timer that was armed when the connection
			// started to have fired.
			time.Sleep(20 * time.Millisecond)

			if err := a.writeData(1, true, []byte("body")); err != nil {
				t.Fatalf("connection %d: body: %v", i, err)
			}

			a.flush()

			_ = a.c.SetReadDeadline(time.Now().Add(10 * time.Second))

			for {
				fr, err := ReadFrameFrom(a.br)
				if err != nil {
					t.Fatalf("connection %d: reading the answer: %v", i, err)
				}

				kind, stream := fr.Type(), fr.Stream()

				var code ErrorCode
				if kind == FrameResetStream {
					code = fr.Body().(*RstStream).Code()
				}

				ReleaseFrameHeader(fr)

				if kind == FrameResetStream && stream == 1 {
					t.Fatalf("connection %d: server reset a request that was still arriving, with %s", i, code)
				}

				if kind == FrameHeaders && stream == 1 {
					return
				}
			}
		}()
	}
}

// TestServerWaitsForEndHeaders covers RFC 7540 6.2 and 6.10: a HEADERS frame
// that sets END_STREAM but not END_HEADERS half-closes the stream while its
// header block is still arriving in CONTINUATION frames. The request is not
// complete until END_HEADERS, and answering at END_STREAM hands the handler a
// request whose headers are only half decoded.
func TestServerWaitsForEndHeaders(t *testing.T) {
	type seen struct {
		late string
		ok   bool
	}

	got := make(chan seen, 4)

	addr := newConcurrencyServer(t, ServerConfig{PingInterval: -1},
		func(ctx *fasthttp.RequestCtx) {
			got <- seen{late: string(ctx.Request.Header.Peek("x-late")), ok: true}
			ctx.SetBodyString("ok")
		})

	a := dialAttacker(t, addr)

	// END_STREAM here, END_HEADERS only on the CONTINUATION that follows.
	if err := a.writeHeaders(1, true, false, requestFields(addr)); err != nil {
		t.Fatal(err)
	}

	a.flush()

	// Give the server every chance to answer early, which is the bug.
	time.Sleep(100 * time.Millisecond)

	select {
	case s := <-got:
		t.Fatalf("the handler ran before END_HEADERS, seeing x-late=%q", s.late)
	default:
	}

	if err := a.writeContinuationFields(1, true, []hdr{{"x-late", "present"}}); err != nil {
		t.Fatal(err)
	}

	a.flush()

	select {
	case s := <-got:
		if s.late != "present" {
			t.Errorf("handler saw x-late=%q, want %q: the header block was not "+
				"fully decoded before the request was dispatched", s.late, "present")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// writeSettingsWith sends a SETTINGS frame carrying st.
func (a *attacker) writeSettingsWith(st *Settings) error {
	fr := AcquireFrameHeader()

	frSt := AcquireFrame(FrameSettings).(*Settings)
	st.CopyTo(frSt)

	fr.SetBody(frSt)

	if err := a.write(fr); err != nil {
		return err
	}

	a.flush()

	return nil
}

// TestServerRejectsFrameOverItsOwnMaxFrameSize covers RFC 7540 4.2: a frame
// larger than the SETTINGS_MAX_FRAME_SIZE the receiver advertised is an error.
// The limit is the receiver's own, not the sender's: a peer that says it can
// receive 1 MiB frames has said nothing about what it may send.
func TestServerRejectsFrameOverItsOwnMaxFrameSize(t *testing.T) {
	addr, _ := newAttackServer(t, ServerConfig{PingInterval: -1})

	a := dialAttacker(t, addr)
	d := a.drain()

	// We can receive 1 MiB frames. The server still only accepts 16 KiB ones.
	st := &Settings{}
	st.Reset()
	st.SetMaxFrameSize(1 << 20)

	if err := a.writeSettingsWith(st); err != nil {
		t.Fatalf("writing the settings: %v", err)
	}

	// Give the server time to apply them, so the test fails on the size check
	// rather than on a race with it.
	time.Sleep(100 * time.Millisecond)

	if err := a.writeRaw(byte(FrameData), 0, 1, make([]byte, 256*1024)); err != nil &&
		!isClosedConn(err) {
		t.Fatalf("writing the oversized frame: %v", err)
	}

	if !d.wait(5 * time.Second) {
		t.Error("server kept the connection open after an oversized frame")
	}

	if !d.gotGoAway.Load() {
		t.Fatal("no GOAWAY for a frame over the advertised maximum size")
	}

	if code := ErrorCode(d.goaway.Load()); code != FrameSizeError {
		t.Errorf("goaway code = %s, want %s", code, FrameSizeError)
	}
}

// TestServerRejectsOversizedFrameBeforeSettings covers the same limit before
// the peer has sent any SETTINGS. Until then the server has nothing to go on
// but its own advertised value, and a connection with no limit at all lets one
// frame allocate up to the 16 MiB the length field can express.
func TestServerRejectsOversizedFrameBeforeSettings(t *testing.T) {
	addr, _ := newAttackServer(t, ServerConfig{PingInterval: -1})

	c, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{H2TLSProto},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Write(http2Preface); err != nil {
		t.Fatalf("writing the preface: %v", err)
	}

	a := &attacker{t: t, c: c, br: bufio.NewReader(c), bw: bufio.NewWriter(c), addr: addr}
	d := a.drain()

	// No SETTINGS of our own, straight to a frame the server never said it
	// would accept.
	if err := a.writeRaw(byte(FrameData), 0, 1, make([]byte, 256*1024)); err != nil &&
		!isClosedConn(err) {
		t.Fatalf("writing the oversized frame: %v", err)
	}

	if !d.wait(5 * time.Second) {
		t.Error("server kept the connection open after an oversized frame")
	}

	if !d.gotGoAway.Load() {
		t.Fatal("no GOAWAY for a frame over the advertised maximum size")
	}

	if code := ErrorCode(d.goaway.Load()); code != FrameSizeError {
		t.Errorf("goaway code = %s, want %s", code, FrameSizeError)
	}
}
