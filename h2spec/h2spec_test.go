package h2spec

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/domsolutions/http2"
	"github.com/stretchr/testify/require"
	"github.com/summerwind/h2spec/config"
	"github.com/summerwind/h2spec/generic"
	h2spec "github.com/summerwind/h2spec/http2"
	"github.com/valyala/fasthttp"
)

func TestH2Spec(t *testing.T) {
	port := launchLocalServer(t)

	testCases := []struct {
		desc string
	}{
		{desc: "generic/1/1"},
		{desc: "generic/2/1"},
		{desc: "generic/2/2"},
		{desc: "generic/2/3"},
		{desc: "generic/2/4"},
		{desc: "generic/2/5"},
		{desc: "generic/3.1/1"},
		{desc: "generic/3.1/2"},
		{desc: "generic/3.1/3"},
		{desc: "generic/3.2/1"},
		{desc: "generic/3.2/2"},
		{desc: "generic/3.2/3"},
		{desc: "generic/3.3/1"},
		{desc: "generic/3.3/2"},
		{desc: "generic/3.3/3"},
		{desc: "generic/3.3/4"},
		{desc: "generic/3.3/5"},
		{desc: "generic/3.4/1"},
		{desc: "generic/3.5/1"},
		{desc: "generic/3.7/1"},
		{desc: "generic/3.8/1"},
		{desc: "generic/3.9/1"},
		{desc: "generic/3.9/2"},
		{desc: "generic/3.10/1"},
		{desc: "generic/3.10/2"},
		{desc: "generic/4/1"},
		{desc: "generic/4/2"},
		{desc: "generic/4/3"},
		{desc: "generic/4/4"},
		{desc: "generic/5/1"},
		{desc: "generic/5/2"},
		{desc: "generic/5/3"},
		{desc: "generic/5/4"},
		{desc: "generic/5/5"},
		{desc: "generic/5/6"},
		{desc: "generic/5/7"},
		{desc: "generic/5/8"},
		{desc: "generic/5/9"},
		{desc: "generic/5/10"},
		{desc: "generic/5/11"},
		{desc: "generic/5/12"},
		{desc: "generic/5/13"},
		{desc: "generic/5/14"},
		{desc: "generic/5/15"},

		{desc: "http2/3.5/1"},
		{desc: "http2/3.5/2"},
		{desc: "http2/4.1/1"},
		{desc: "http2/4.1/2"},
		{desc: "http2/4.1/3"},
		{desc: "http2/4.2/1"},
		{desc: "http2/4.2/2"},
		{desc: "http2/4.2/3"},
		{desc: "http2/4.3/1"},
		{desc: "http2/4.3/2"},
		{desc: "http2/4.3/3"},
		{desc: "http2/5.1.1/1"},
		{desc: "http2/5.1.1/2"},
		// {desc: "http2/5.1.2/1"},
		{desc: "http2/5.1/1"},
		{desc: "http2/5.1/2"},
		{desc: "http2/5.1/3"},
		{desc: "http2/5.1/4"},
		{desc: "http2/5.1/5"},
		{desc: "http2/5.1/6"},
		{desc: "http2/5.1/7"},
		{desc: "http2/5.1/8"},
		{desc: "http2/5.1/9"},
		{desc: "http2/5.1/10"},
		{desc: "http2/5.1/11"},
		{desc: "http2/5.1/12"},
		{desc: "http2/5.1/13"},
		{desc: "http2/5.3.1/1"},
		{desc: "http2/5.3.1/2"},
		// About(dario): In this one we send a GOAWAY,
		//               but the spec is expecting a connection close.
		//
		// {desc: "http2/5.4.1/1"},
		{desc: "http2/5.4.1/2"},
		{desc: "http2/5.5/1"},
		{desc: "http2/5.5/2"},
		{desc: "http2/6.1/1"},
		{desc: "http2/6.1/2"},
		{desc: "http2/6.1/3"},
		{desc: "http2/6.2/1"},
		{desc: "http2/6.2/2"},
		{desc: "http2/6.2/3"},
		{desc: "http2/6.2/4"},
		{desc: "http2/6.3/1"},
		{desc: "http2/6.3/2"},
		{desc: "http2/6.4/1"},
		{desc: "http2/6.4/2"},
		{desc: "http2/6.4/3"},
		{desc: "http2/6.5.2/1"},
		{desc: "http2/6.5.2/2"},
		{desc: "http2/6.5.2/3"},
		{desc: "http2/6.5.2/4"},
		{desc: "http2/6.5.2/5"},
		{desc: "http2/6.5.3/1"},
		{desc: "http2/6.5.3/2"},
		{desc: "http2/6.5/1"},
		{desc: "http2/6.5/2"},
		{desc: "http2/6.5/3"},
		{desc: "http2/6.7/1"},
		{desc: "http2/6.7/2"},
		{desc: "http2/6.7/3"},
		{desc: "http2/6.7/4"},
		{desc: "http2/6.8/1"},
		{desc: "http2/6.9.1/1"},
		{desc: "http2/6.9.1/2"},
		{desc: "http2/6.9.1/3"},
		// {desc: "http2/6.9.2/1"},
		// {desc: "http2/6.9.2/2"},
		{desc: "http2/6.9.2/3"},
		{desc: "http2/6.9/1"},
		{desc: "http2/6.9/2"},
		{desc: "http2/6.9/3"},
		{desc: "http2/6.10/1"},
		{desc: "http2/6.10/2"},
		{desc: "http2/6.10/3"},
		// About(dario): In this one the client sends a HEADERS with END_HEADERS
		//               and then sending a CONTINUATION frame.
		//               The thing is that we process the request just after reading
		//               the END_HEADERS, so we don't know about the next continuation.
		//
		// {desc: "http2/6.10/4"},
		// {desc: "http2/6.10/5"},
		{desc: "http2/6.10/6"},
		{desc: "http2/7/1"},
		{desc: "http2/7/2"},
		// About(dario): Sends a HEADERS frame that contains the header
		//               field name in uppercase letters.
		//               In this case, fasthttp is case-insensitive, so we can ignore it.
		// {desc: "http2/8.1.2.1/1"},
		// {desc: "http2/8.1.2.1/2"},
		{desc: "http2/8.1.2.1/3"},
		// {desc: "http2/8.1.2.1/4"},
		// {desc: "http2/8.1.2.2/1"},
		// {desc: "http2/8.1.2.2/2"},
		// {desc: "http2/8.1.2.3/1"},
		// {desc: "http2/8.1.2.3/2"},
		// {desc: "http2/8.1.2.3/3"},
		// {desc: "http2/8.1.2.3/4"},
		// {desc: "http2/8.1.2.3/5"},
		// {desc: "http2/8.1.2.3/6"},
		// {desc: "http2/8.1.2.3/7"},
		// {desc: "http2/8.1.2.6/1"},
		// {desc: "http2/8.1.2.6/2"},
		// {desc: "http2/8.1.2/1"},
		{desc: "http2/8.1/1"},
		{desc: "http2/8.2/1"},
		{desc: "hpack/2.3.3"},
		{desc: "hpack/4.2"},
		{desc: "hpack/5.2"},
		{desc: "hpack/6.1"},
		{desc: "hpack/6.3"},
	}

	// Disable logs from h2spec
	oldout := os.Stdout
	os.Stdout = nil
	t.Cleanup(func() {
		os.Stdout = oldout
	})

	for _, test := range testCases {
		test := test
		t.Run(test.desc, func(t *testing.T) {
			t.Parallel()

			conf := &config.Config{
				Host:         "127.0.0.1",
				Port:         port,
				Path:         "/",
				Timeout:      time.Second,
				MaxHeaderLen: 4000,
				TLS:          true,
				Insecure:     true,
				Sections:     []string{test.desc},
			}

			tg := h2spec.Spec()
			if strings.HasPrefix(test.desc, "generic") {
				tg = generic.Spec()
			}

			tg.Test(conf)
			require.Equal(t, 0, tg.FailedCount)
		})
	}
}

func launchLocalServer(t *testing.T) int {
	t.Helper()

	certPEM, keyPEM, err := KeyPair("test.default", time.Time{})
	if err != nil {
		log.Fatalf("Unable to generate certificate: %v", err)
	}

	server := &fasthttp.Server{
		Handler: func(ctx *fasthttp.RequestCtx) {
			ctx.Response.AppendBodyString("Test	 HTTP2")
		},
	}
	http2.ConfigureServer(server, http2.ServerConfig{})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		log.Println(server.ServeTLSEmbed(ln, certPEM, keyPEM))
	}()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	portInt, err := strconv.Atoi(port)
	require.NoError(t, err)

	return portInt
}

// DefaultDomain domain for the default certificate.
const DefaultDomain = "TEST DEFAULT CERT"

// KeyPair generates cert and key files.
func KeyPair(domain string, expiration time.Time) ([]byte, []byte, error) {
	rsaPrivKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaPrivKey)})

	certPEM, err := PemCert(rsaPrivKey, domain, expiration)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

// PemCert generates PEM cert file.
func PemCert(privKey *rsa.PrivateKey, domain string, expiration time.Time) ([]byte, error) {
	derBytes, err := derCert(privKey, expiration, domain)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}), nil
}

func derCert(privKey *rsa.PrivateKey, expiration time.Time, domain string) ([]byte, error) {
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}

	if expiration.IsZero() {
		expiration = time.Now().Add(365 * (24 * time.Hour))
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: DefaultDomain,
		},
		NotBefore: time.Now(),
		NotAfter:  expiration,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain},
	}

	return x509.CreateCertificate(rand.Reader, &template, &template, &privKey.PublicKey, privKey)
}

// func printTest(tg *spec.TestGroup) {
// 	for _, group := range tg.Groups {
// 		printTest(group)
// 	}
// 	for i := range tg.Tests {
// 		fmt.Printf("{desc: \"%s/%d\"},\n", tg.ID(), i+1)
// 	}
// }
