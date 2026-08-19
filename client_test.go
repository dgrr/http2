package http2

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/valyala/fasthttp"
)

// TestClientServerRoundTrip drives this library's HTTP/2 client against its own
// HTTP/2 server over a real TLS connection and issues several sequential
// requests on the same connection. It guards connection reuse: a client that
// sends a WINDOW_UPDATE after receiving a response must not cause the server to
// tear down the connection when the stream has already closed (RFC 7540 5.1).
func TestClientServerRoundTrip(t *testing.T) {
	const body = `{"coin":"BTC","levels":[[{"px":"64041.0","sz":"12.5"}],[{"px":"64042.0","sz":"3.2"}]]}`

	certPEM, keyPEM := testKeyPair(t)

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			if !ctx.IsPost() || string(ctx.Path()) != "/info" {
				ctx.SetStatusCode(fasthttp.StatusNotFound)
				return
			}
			ctx.SetContentType("application/json")
			ctx.SetBodyString(body)
		},
	}
	ConfigureServer(server, ServerConfig{})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() { _ = server.ServeTLSEmbed(ln, certPEM, keyPEM) }()

	addr := ln.Addr().String()

	hc := &fasthttp.HostClient{
		Addr:      addr,
		IsTLS:     true,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	}

	// Give the listener a moment to come up before dialing.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err = ConfigureClient(hc, ClientOpts{}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ConfigureClient: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Several requests on the same client exercise connection reuse: each one
	// opens a new stream on the connection established above.
	for i := 0; i < 5; i++ {
		req := fasthttp.AcquireRequest()
		resp := fasthttp.AcquireResponse()

		req.SetRequestURI("https://" + addr + "/info")
		req.Header.SetMethod(fasthttp.MethodPost)
		req.Header.SetContentType("application/json")
		req.SetBodyString(`{"type":"l2Book","coin":"BTC"}`)

		if err := hc.Do(req, resp); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}

		if resp.StatusCode() != fasthttp.StatusOK {
			t.Fatalf("request %d: status %d", i, resp.StatusCode())
		}
		if string(resp.Body()) != body {
			t.Fatalf("request %d: unexpected body %q", i, resp.Body())
		}

		fasthttp.ReleaseRequest(req)
		fasthttp.ReleaseResponse(resp)
	}
}

func testKeyPair(t testing.TB) ([]byte, []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	return certPEM, keyPEM
}
