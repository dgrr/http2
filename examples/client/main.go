package main

import (
	"encoding/json"
	"fmt"
	"github.com/dgrr/http2"
	"log"
	"sync"

	"github.com/dgrr/http2/fasthttp2"
	"github.com/valyala/fasthttp"
)

func main() {
	c := &fasthttp.HostClient{
		Addr:  "api.binance.com:443",
		IsTLS: true,
	}

	if err := http2.ConfigureClient(c); err != nil {
		panic(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		req := fasthttp.AcquireRequest()
		res := fasthttp.AcquireResponse()

		res.Reset()

		req.Header.SetMethod("GET")
		// TODO: Use SetRequestURI
		req.URI().Update("https://api.binance.com/api/v3/exchangeInfo")

		err := c.Do(req, res)
		if err != nil {
			log.Fatalln(err)
		}

		body := res.Body()

		fmt.Printf("%d: %d\n", res.Header.StatusCode(), len(body))
		res.Header.VisitAll(func(k, v []byte) {
			fmt.Printf("%s: %s\n", k, v)
		})

		a := make(map[string]interface{})
		if err = json.Unmarshal(body, &a); err != nil {
			panic(err)
		}

		fmt.Println("------------------------")
	}

	// fmt.Printf("%s\n", body)
}
