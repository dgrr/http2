package http2

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
	xhttp2 "golang.org/x/net/http2"
)

// Conformance to h2spec says the server follows the RFC. These tests say the
// two ends talk to something that is not themselves: the HTTP/2 implementation
// bundled with net/http, which is the peer most real deployments will meet.

// TestInteropClientAgainstStdlibServer drives this library's client at the
// standard library's HTTP/2 server.
func TestInteropClientAgainstStdlibServer(t *testing.T) {
	const big = 512 << 10

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("server saw %s, want HTTP/2", r.Proto)
		}

		switch r.URL.Path {
		case "/echo":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading request body: %v", err)
			}

			w.Header().Set("X-Seen-Method", r.Method)
			w.Header().Set("X-Seen-Custom", r.Header.Get("X-Custom"))
			_, _ = w.Write(body)
		case "/big":
			// Larger than the initial flow-control window, so the response
			// only completes if WINDOW_UPDATE handling works.
			_, _ = w.Write(bytes.Repeat([]byte("x"), big))
		case "/status":
			w.WriteHeader(http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))

	srv.EnableHTTP2 = true
	srv.StartTLS()

	t.Cleanup(srv.Close)

	addr := srv.Listener.Addr().String()

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{}); err != nil {
		t.Fatalf("ConfigureClient: %v", err)
	}

	do := func(method, path, body string, set func(*fasthttp.Request)) *fasthttp.Response {
		t.Helper()

		req := fasthttp.AcquireRequest()
		defer fasthttp.ReleaseRequest(req)

		req.SetRequestURI("https://" + addr + path)
		req.Header.SetMethod(method)
		req.SetBodyString(body)

		if set != nil {
			set(req)
		}

		res := fasthttp.AcquireResponse()

		if err := hc.Do(req, res); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}

		return res
	}

	t.Run("echo", func(t *testing.T) {
		res := do(fasthttp.MethodPost, "/echo", "hello over h2", func(req *fasthttp.Request) {
			req.Header.Set("X-Custom", "a value")
		})
		defer fasthttp.ReleaseResponse(res)

		if got := string(res.Body()); got != "hello over h2" {
			t.Errorf("body = %q, want %q", got, "hello over h2")
		}

		if got := string(res.Header.Peek("X-Seen-Method")); got != fasthttp.MethodPost {
			t.Errorf("method seen by the server = %q, want POST", got)
		}

		if got := string(res.Header.Peek("X-Seen-Custom")); got != "a value" {
			t.Errorf("custom header seen by the server = %q, want %q", got, "a value")
		}
	})

	t.Run("body larger than the window", func(t *testing.T) {
		res := do(fasthttp.MethodGet, "/big", "", nil)
		defer fasthttp.ReleaseResponse(res)

		if len(res.Body()) != big {
			t.Errorf("body = %d bytes, want %d", len(res.Body()), big)
		}
	})

	t.Run("status code", func(t *testing.T) {
		res := do(fasthttp.MethodGet, "/status", "", nil)
		defer fasthttp.ReleaseResponse(res)

		if res.StatusCode() != http.StatusTeapot {
			t.Errorf("status = %d, want %d", res.StatusCode(), http.StatusTeapot)
		}
	})
}

// TestInteropStdlibClientAgainstServer drives the standard library's HTTP/2
// client at this library's server, including several streams at once on the
// same connection.
func TestInteropStdlibClientAgainstServer(t *testing.T) {
	const big = 512 << 10

	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			switch string(ctx.Path()) {
			case "/echo":
				ctx.Response.Header.Set("X-Seen-Method", string(ctx.Method()))
				ctx.Response.Header.Set("X-Seen-Custom", string(ctx.Request.Header.Peek("X-Custom")))
				ctx.SetBody(ctx.PostBody())
			case "/big":
				ctx.SetBody(bytes.Repeat([]byte("x"), big))
			case "/status":
				ctx.SetStatusCode(http.StatusTeapot)
			default:
				ctx.SetStatusCode(http.StatusNotFound)
			}
		},
		Logger: discardLogger{},
	}
	ConfigureServer(server, ServerConfig{})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()
	waitForServer(t, addr)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			ForceAttemptHTTP2: true,
		},
		Timeout: 30 * time.Second,
	}

	t.Cleanup(client.CloseIdleConnections)

	get := func(t *testing.T, method, path, body string, set func(*http.Request)) *http.Response {
		t.Helper()

		req, err := http.NewRequest(method, "https://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}

		if set != nil {
			set(req)
		}

		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}

		if res.ProtoMajor != 2 {
			t.Fatalf("client negotiated %s, want HTTP/2", res.Proto)
		}

		return res
	}

	t.Run("echo", func(t *testing.T) {
		res := get(t, http.MethodPost, "/echo", "hello from net/http", func(req *http.Request) {
			req.Header.Set("X-Custom", "a value")
		})
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}

		if string(body) != "hello from net/http" {
			t.Errorf("body = %q, want %q", body, "hello from net/http")
		}

		if got := res.Header.Get("X-Seen-Method"); got != http.MethodPost {
			t.Errorf("method seen by the server = %q, want POST", got)
		}

		if got := res.Header.Get("X-Seen-Custom"); got != "a value" {
			t.Errorf("custom header seen by the server = %q, want %q", got, "a value")
		}
	})

	t.Run("body larger than the window", func(t *testing.T) {
		res := get(t, http.MethodGet, "/big", "", nil)
		defer res.Body.Close()

		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}

		if len(body) != big {
			t.Errorf("body = %d bytes, want %d", len(body), big)
		}
	})

	t.Run("status code", func(t *testing.T) {
		res := get(t, http.MethodGet, "/status", "", nil)
		defer res.Body.Close()

		if res.StatusCode != http.StatusTeapot {
			t.Errorf("status = %d, want %d", res.StatusCode, http.StatusTeapot)
		}
	})

	// net/http multiplexes these onto the one connection it already has, so
	// this exercises concurrent streams rather than concurrent connections.
	t.Run("concurrent streams", func(t *testing.T) {
		const streams = 64

		var wg sync.WaitGroup

		wg.Add(streams)

		errs := make(chan error, streams)

		for i := 0; i < streams; i++ {
			go func(i int) {
				defer wg.Done()

				want := fmt.Sprintf("stream %d", i)

				req, err := http.NewRequest(http.MethodPost, "https://"+addr+"/echo", strings.NewReader(want))
				if err != nil {
					errs <- err
					return
				}

				res, err := client.Do(req)
				if err != nil {
					errs <- err
					return
				}

				defer res.Body.Close()

				body, err := io.ReadAll(res.Body)
				if err != nil {
					errs <- err
					return
				}

				if string(body) != want {
					errs <- fmt.Errorf("stream %d got %q", i, body)
				}
			}(i)
		}

		wg.Wait()
		close(errs)

		for err := range errs {
			t.Error(err)
		}
	})
}

// TestInteropStdlibClientSendsTrailers covers a request whose body is followed
// by a trailing header block: gRPC does this on every call, and so does any
// client streaming a body it can only checksum at the end. The trailer fields
// arrive as request headers, which is the closest fasthttp's request has to a
// place to put them.
func TestInteropStdlibClientSendsTrailers(t *testing.T) {
	certPEM, keyPEM := testKeyPair(t)

	type seen struct {
		body    string
		trailer string
	}

	got := make(chan seen, 1)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			select {
			case got <- seen{
				body:    string(ctx.Request.Body()),
				trailer: string(ctx.Request.Header.Peek("X-Checksum")),
			}:
			default:
			}

			ctx.SetBodyString("ok")
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

	tr := &xhttp2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	t.Cleanup(tr.CloseIdleConnections)

	pr, pw := io.Pipe()

	req, err := http.NewRequest(http.MethodPost, "https://"+addr+"/", pr)
	if err != nil {
		t.Fatal(err)
	}

	// Declaring the trailer up front is what makes net/http send the fields
	// after the body instead of with the headers.
	req.Trailer = http.Header{"X-Checksum": nil}

	go func() {
		_, _ = pw.Write([]byte("payload"))

		req.Trailer.Set("X-Checksum", "abc123")

		_ = pw.Close()
	}()

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("request with trailers: %v", err)
	}

	defer func() { _ = res.Body.Close() }()

	if _, err := io.ReadAll(res.Body); err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}

	select {
	case s := <-got:
		if s.body != "payload" {
			t.Errorf("body = %q, want %q", s.body, "payload")
		}

		if s.trailer != "abc123" {
			t.Errorf("trailer field = %q, want %q", s.trailer, "abc123")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// TestInteropStdlibClientReadsStreamedBody covers a response the handler
// streams rather than buffers, which is the path fasthttp takes for
// SetBodyStream and ServeFile. It is metered against the peer's windows a frame
// at a time, so this checks the whole body still arrives, in order, against a
// client that enforces flow control.
func TestInteropStdlibClientReadsStreamedBody(t *testing.T) {
	const size = 2 << 20

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

	tr := &xhttp2.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	t.Cleanup(tr.CloseIdleConnections)

	res, err := client.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading a streamed body: %v", err)
	}

	if len(body) != size {
		t.Fatalf("body = %d bytes, want %d", len(body), size)
	}

	if want := bytes.Repeat([]byte("x"), size); !bytes.Equal(body, want) {
		t.Error("the streamed body came back altered")
	}
}
