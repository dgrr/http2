package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/domsolutions/http2/benchmark/common"
	"golang.org/x/net/http2"
)

func main() {
	certData, priv, err := common.GenerateTestCertificate("localhost:8443")
	if err != nil {
		log.Fatalln(err)
	}

	srv := &http.Server{
		Addr:        ":8443",
		ReadTimeout: time.Second * 3,
		TLSConfig: &tls.Config{
			ServerName: "locahost",
		},
		Handler: http.HandlerFunc(requestHandler),
	}

	cert, err := tls.X509KeyPair(certData, priv)
	if err != nil {
		panic(err)
	}

	srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)

	err = http2.ConfigureServer(srv, &http2.Server{})
	if err != nil {
		panic(err)
	}

	ln, err := tls.Listen("tcp", ":8443", srv.TLSConfig)
	if err != nil {
		panic(err)
	}

	err = srv.Serve(ln)
	if err != nil {
		panic(err)
	}
}

func requestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		io.Copy(w, r.Body)
		io.WriteString(w, "\n")

		r.Body.Close()

		return
	}

	fmt.Fprintf(w, "Hello 21th century!\n")
}
