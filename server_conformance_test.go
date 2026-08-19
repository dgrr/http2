package http2

import (
	"encoding/binary"
	"testing"
	"time"
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
