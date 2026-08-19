package main

import (
	"log"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

func main() {
	cert, priv, err := GenerateTestCertificate("localhost:8443")
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

	http2.ConfigureServer(s, http2.ServerConfig{})

	err = s.ListenAndServeTLS(":8443", "", "")
	if err != nil {
		log.Fatalln(err)
	}
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(res)

	ctx.Request.CopyTo(req)

	req.Header.SetProtocol("HTTP/1.1")
	req.SetRequestURI("http://localhost:8080" + string(ctx.RequestURI()))

	if err := fasthttp.Do(req, res); err != nil {
		ctx.Error("gateway error", fasthttp.StatusBadGateway)
		return
	}

	res.CopyTo(&ctx.Response)
}
