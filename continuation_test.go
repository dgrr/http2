package http2

import (
	"strings"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// A header block larger than SETTINGS_MAX_FRAME_SIZE has to go out as a HEADERS
// frame followed by CONTINUATION frames (RFC 7540 6.2). Both ends of this
// library encoded the whole block into one frame regardless of its size, which
// any peer that enforces the limit rejects, this library's own server included.

// largeHeaderValue is comfortably past the 16 KiB minimum frame size and well
// inside the default header list limit.
var largeHeaderValue = strings.Repeat("abcdefgh", 4*1024)

// TestClientSplitsLargeRequestHeaders sends a request whose header block does
// not fit one frame.
func TestClientSplitsLargeRequestHeaders(t *testing.T) {
	seen := make(chan string, 1)

	addr := newConcurrencyServer(t, ServerConfig{PingInterval: -1},
		func(ctx *fasthttp.RequestCtx) {
			select {
			case seen <- string(ctx.Request.Header.Peek("x-big")):
			default:
			}

			ctx.SetBodyString("ok")
		})

	hc := clientFor(t, addr)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodGet)
	req.Header.Set("x-big", largeHeaderValue)

	done := make(chan error, 1)

	go func() { done <- hc.Do(req, res) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a request with a header block over the frame size failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request did not return")
	}

	if res.StatusCode() != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode())
	}

	select {
	case got := <-seen:
		if got != largeHeaderValue {
			t.Errorf("the handler saw %d bytes of x-big, want %d", len(got), len(largeHeaderValue))
		}
	case <-time.After(time.Second):
		t.Fatal("the handler never ran")
	}
}

// TestServerSplitsLargeResponseHeaders is the same block on the way back.
func TestServerSplitsLargeResponseHeaders(t *testing.T) {
	addr := newConcurrencyServer(t, ServerConfig{PingInterval: -1},
		func(ctx *fasthttp.RequestCtx) {
			ctx.Response.Header.Set("x-big", largeHeaderValue)
			ctx.SetBodyString("ok")
		})

	hc := clientFor(t, addr)

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodGet)

	done := make(chan error, 1)

	go func() { done <- hc.Do(req, res) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a response with a header block over the frame size failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request did not return")
	}

	if got := string(res.Header.Peek("x-big")); got != largeHeaderValue {
		t.Errorf("the client saw %d bytes of x-big, want %d", len(got), len(largeHeaderValue))
	}
}
