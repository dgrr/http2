package main

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func main() {
	hostName := "example.com"

	cert, priv, err := configureCert(hostName)
	if err != nil {
		log.Fatalln(err)
	}

	s := &fasthttp.Server{
		Handler: requestHandler,
		Name:    "http2 test",
	}

	http2.ConfigureServer(s, http2.ServerConfig{})

	log.Println("fasthttp", s.ListenAndServeTLSEmbed(":443", cert, priv))
}

func configureCert(hostName string) ([]byte, []byte, error) {
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(hostName),
		Cache:      autocert.DirCache("./certs"),
	}

	cfg := &tls.Config{
		GetCertificate: m.GetCertificate,
		NextProtos: []string{
			acme.ALPNProto,
		},
	}

	s := &http.Server{
		Addr:      ":80",
		Handler:   m.HTTPHandler(nil),
		TLSConfig: cfg,
	}
	go s.ListenAndServe()

	time.Sleep(time.Second * 10)
	s.Shutdown(context.Background())

	data, err := m.Cache.Get(context.Background(), hostName)
	if err != nil {
		return nil, nil, err
	}

	priv, restBytes := pem.Decode(data)
	cert, _ := pem.Decode(restBytes)

	return pem.EncodeToMemory(cert), pem.EncodeToMemory(priv), nil
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	fmt.Printf("%s\n", ctx.Request.Header.Header())
	if ctx.Request.Header.IsPost() {
		fmt.Fprintf(ctx, "%s\n", ctx.Request.Body())
	} else {
		fmt.Fprintf(ctx, "Hello 21th century!\n")
	}
}
