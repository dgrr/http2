# HTTP2

http2 is a implementation of HTTP/2 protocol for [fasthttp](https://github.com/valyala/fasthttp).

# Example

```go
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
	fmt.Printf("%s\n", ctx.Request.Header.Header())
	if ctx.Request.Header.IsPost() {
		fmt.Fprintf(ctx, "%s\n", ctx.Request.Body())
	} else {
		fmt.Fprintf(ctx, "Hello 21th century!\n")
	}
}
```
