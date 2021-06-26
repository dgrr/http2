# HTTP2

http2 is a implementation of HTTP/2 protocol for [fasthttp](https://github.com/valyala/fasthttp).

# Download

```bash
go get github.com/dgrr/http2@v0.0.3
```

# Client example

```go
package main

import (
        "fmt"
        "log"

        "github.com/dgrr/http2/fasthttp2"
        "github.com/valyala/fasthttp"
)

func main() {
        hc := &fasthttp.HostClient{
                Addr:  "api.binance.com:443",
                IsTLS: true,
        }

        if err := fasthttp2.ConfigureClient(hc); err != nil {
                log.Printf("%s doesn't support http/2\n", hc.Addr)
        }

        statusCode, body, err := hc.Get(nil, "https://api.binance.com/api/v3/time")
        if err != nil {
                log.Fatalln(err)
        }

        fmt.Printf("%d: %s\n", statusCode, body)
}
```
