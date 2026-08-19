package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A response the handler streams used to go straight into the writer, whole,
// no matter what the peer had said it could take. These pin it to the windows.

// streamingServer serves size bytes from a body stream, which is the path
// fasthttp takes for SetBodyStream, ServeFile and anything else the handler
// does not buffer.
func streamingServer(t *testing.T, size int) string {
	t.Helper()

	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetBodyStream(bytes.NewReader(bytes.Repeat([]byte("x"), size)), size)
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
	waitForServer(t, addr)

	return addr
}

// dialWithWindow opens a raw connection that advertises streamWindow as its
// SETTINGS_INITIAL_WINDOW_SIZE and grants connWindowExtra on top of the 65535
// every connection starts with.
func dialWithWindow(t *testing.T, addr string, streamWindow, connWindowExtra int32) *attacker {
	t.Helper()

	c, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{H2TLSProto},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	t.Cleanup(func() { _ = c.Close() })

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
	st.SetMaxWindowSize(uint32(streamWindow))

	if err := Handshake(true, a.bw, st, connWindowExtra); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	a.flush()

	return a
}

// readData counts the response DATA bytes until the connection goes quiet.
func readData(t *testing.T, a *attacker, quiet time.Duration) int {
	t.Helper()

	total := 0

	for {
		_ = a.c.SetReadDeadline(time.Now().Add(quiet))

		fr, err := ReadFrameFrom(a.br)
		if err != nil {
			return total
		}

		if fr.Type() == FrameData {
			total += fr.Len()
		}

		ReleaseFrameHeader(fr)
	}
}

// TestServerStopsStreamedBodyAtStreamWindow checks a streamed response is
// metered against the peer's stream window like a buffered one is.
func TestServerStopsStreamedBodyAtStreamWindow(t *testing.T) {
	const (
		size         = 1 << 20
		streamWindow = 4096
	)

	addr := streamingServer(t, size)

	a := dialWithWindow(t, addr, streamWindow, 1<<20)

	if err := a.writeHeaders(1, true, true, requestFields(addr)); err != nil {
		t.Fatal(err)
	}

	a.flush()

	if n := readData(t, a, time.Second); n != streamWindow {
		t.Errorf("server sent %d bytes of a streamed body with a %d byte stream window", n, streamWindow)
	}
}

// TestServerStopsStreamedBodyAtConnectionWindow does the same for the
// connection window, which every connection starts at 65535 whatever the
// SETTINGS say.
func TestServerStopsStreamedBodyAtConnectionWindow(t *testing.T) {
	const size = 1 << 20

	addr := streamingServer(t, size)

	// A large stream window leaves the connection window as the only limit. The
	// handshake has to grant something, so it grants one byte.
	a := dialWithWindow(t, addr, 1<<20, 1)

	if err := a.writeHeaders(1, true, true, requestFields(addr)); err != nil {
		t.Fatal(err)
	}

	a.flush()

	want := int(defaultWindowSize) + 1

	if n := readData(t, a, time.Second); n != want {
		t.Errorf("server sent %d bytes of a streamed body with a %d byte connection window", n, want)
	}
}

// TestServerFinishesStreamedBodyAsWindowsOpen checks the held back part of a
// streamed body goes out once the peer makes room, and that the response ends
// with END_STREAM rather than stalling half sent.
func TestServerFinishesStreamedBodyAsWindowsOpen(t *testing.T) {
	const (
		size         = 512 << 10
		streamWindow = 16 << 10
	)

	addr := streamingServer(t, size)

	a := dialWithWindow(t, addr, streamWindow, 1)

	if err := a.writeHeaders(1, true, true, requestFields(addr)); err != nil {
		t.Fatal(err)
	}

	a.flush()

	total := 0
	ended := false

	for !ended {
		_ = a.c.SetReadDeadline(time.Now().Add(10 * time.Second))

		fr, err := ReadFrameFrom(a.br)
		if err != nil {
			t.Fatalf("after %d of %d bytes: %v", total, size, err)
		}

		if fr.Type() == FrameData {
			total += fr.Len()
			ended = fr.Flags().Has(FlagEndStream)

			// Hand back what was used on both levels, the way a peer that is
			// keeping up would.
			var payload [4]byte

			binary.BigEndian.PutUint32(payload[:], uint32(fr.Len()))

			if err := a.writeRaw(byte(FrameWindowUpdate), 0, 0, payload[:]); err != nil {
				t.Fatal(err)
			}

			if err := a.writeRaw(byte(FrameWindowUpdate), 0, 1, payload[:]); err != nil {
				t.Fatal(err)
			}
		}

		ReleaseFrameHeader(fr)
	}

	if total != size {
		t.Errorf("received %d bytes of a %d byte streamed body", total, size)
	}
}
