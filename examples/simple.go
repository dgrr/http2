package main

import (
	"fmt"
	"log"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

func main() {
	cert, priv, err := GenerateTestCertificate("localhost:8080")
	if err != nil {
		log.Fatalln(err)
	}

	s := &fasthttp.Server{
		Handler: requestHandler,
		Name:    "http2 test",
	}
	err = s.AppendCertEmbed(cert, priv)
	if err != nil {
		log.Fatalln(err)
	}

	http2.ConfigureServer(s)

	err = s.ListenAndServeTLS(":8443", "", "")
	if err != nil {
		log.Fatalln(err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	fmt.Fprintf(ctx, "Hello world!\n")
}
