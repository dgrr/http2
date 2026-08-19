package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"

	"github.com/valyala/fasthttp"
)

// These give CI something to compare across commits. The end-to-end ones run
// over a real TLS connection to a real server, so they move when anything in
// the request path regresses, not just when the piece under test does.

func benchServer(b *testing.B) string {
	b.Helper()

	certPEM, keyPEM := testKeyPair(b)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.SetContentType("text/plain")
			ctx.SetBodyString("hello")
		},
		Logger: discardLogger{},
	}
	ConfigureServer(server, ServerConfig{PingInterval: -1})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = ln.Close() })

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	// Wait for the listener the same way the tests do.
	for i := 0; i < 200; i++ {
		hc := &fasthttp.HostClient{
			Addr:      addr,
			IsTLS:     true,
			TLSConfig: &tls.Config{InsecureSkipVerify: true},
		}

		if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err == nil {
			_ = ClientFrom(hc).Close()
			return addr
		}
	}

	b.Fatal("server never came up")

	return ""
}

// BenchmarkRequestSerial measures one request at a time on one connection: the
// latency path, with no concurrency to hide work behind.
func BenchmarkRequestSerial(b *testing.B) {
	addr := benchServer(b)

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = ClientFrom(hc).Close() })

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI("https://" + addr + "/")
	req.Header.SetMethod(fasthttp.MethodGet)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := hc.Do(req, res); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRequestParallel measures throughput with many streams in flight on
// the same connection.
func BenchmarkRequestParallel(b *testing.B) {
	addr := benchServer(b)

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = ClientFrom(hc).Close() })

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodGet)

		for pb.Next() {
			if err := hc.Do(req, res); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkHPACKEncode(b *testing.B) {
	fields := []hdr{
		{":method", "GET"},
		{":path", "/some/reasonably/long/path?with=a&query=string"},
		{":scheme", "https"},
		{":authority", "example.test"},
		{"user-agent", "benchmark/1.0"},
		{"accept", "application/json"},
		{"accept-encoding", "gzip, deflate"},
	}

	hp := &HPACK{}
	hp.Reset()

	hf := &HeaderField{}

	var dst []byte

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dst = dst[:0]

		for _, f := range fields {
			hf.Set(f.key, f.value)
			dst = hp.AppendHeader(dst, hf, false)
		}
	}
}

func BenchmarkHPACKDecode(b *testing.B) {
	enc := &HPACK{}
	enc.Reset()

	hf := &HeaderField{}

	var block []byte

	for _, f := range []hdr{
		{":method", "GET"},
		{":path", "/some/reasonably/long/path?with=a&query=string"},
		{":scheme", "https"},
		{":authority", "example.test"},
		{"user-agent", "benchmark/1.0"},
	} {
		hf.Set(f.key, f.value)
		block = enc.AppendHeader(block, hf, false)
	}

	dec := &HPACK{}
	out := &HeaderField{}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dec.Reset()

		b2 := block
		for len(b2) > 0 {
			var err error

			b2, err = dec.Next(out, b2)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkHuffmanEncode(b *testing.B) {
	src := []byte("/some/reasonably/long/path?with=a&query=string")

	var dst []byte

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		dst = HuffmanEncode(dst[:0], src)
	}
}

func BenchmarkHuffmanDecode(b *testing.B) {
	src := HuffmanEncode(nil, []byte("/some/reasonably/long/path?with=a&query=string"))

	var dst []byte

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		var err error

		dst, err = HuffmanDecode(dst[:0], src)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFrameRoundTrip measures the frame header encode and decode path on
// its own, with no connection involved.
func BenchmarkFrameRoundTrip(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 1024)

	var buf bytes.Buffer

	bw := bufio.NewWriter(&buf)

	// Reused across iterations: a fresh bufio.Reader per iteration allocates a
	// 4 KiB buffer, which would swamp what the benchmark is measuring.
	br := bufio.NewReader(&buf)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		buf.Reset()
		br.Reset(&buf)

		fr := AcquireFrameHeader()
		fr.SetStream(1)

		data := AcquireFrame(FrameData).(*Data)
		data.SetData(payload)
		fr.SetBody(data)

		if _, err := fr.WriteTo(bw); err != nil {
			b.Fatal(err)
		}

		if err := bw.Flush(); err != nil {
			b.Fatal(err)
		}

		ReleaseFrameHeader(fr)

		got, err := ReadFrameFrom(br)
		if err != nil && err != io.EOF {
			b.Fatal(err)
		}

		if got != nil {
			ReleaseFrameHeader(got)
		}
	}
}

// BenchmarkRequestWithBody covers the flow-controlled send path: the body goes
// out in DATA frames metered against the server's windows.
func BenchmarkRequestWithBody(b *testing.B) {
	addr := benchServer(b)

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	if err := ConfigureClient(hc, ClientOpts{PingInterval: -1}); err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = ClientFrom(hc).Close() })

	body := bytes.Repeat([]byte("x"), 8<<10)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		defer fasthttp.ReleaseRequest(req)
		defer fasthttp.ReleaseResponse(res)

		req.SetRequestURI("https://" + addr + "/")
		req.Header.SetMethod(fasthttp.MethodPost)

		for pb.Next() {
			req.SetBody(body)

			if err := hc.Do(req, res); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
