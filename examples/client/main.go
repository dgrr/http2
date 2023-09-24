package main

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/domsolutions/http2"
	"github.com/valyala/fasthttp"
)

func main() {
	c := &fasthttp.HostClient{
		Addr:  "api.binance.com:443",
		IsTLS: true,
	}

	if err := http2.ConfigureClient(c, http2.ClientOpts{}); err != nil {
		panic(err)
	}

	var wg sync.WaitGroup

	reqs := 0

	max := int32(0)
	for i := 0; i < 20; i++ {
		wg.Add(1)

		for atomic.LoadInt32(&max) == 8 {
			time.Sleep(time.Millisecond * 20)
		}
		atomic.AddInt32(&max, 1)

		reqs++

		fmt.Println("reqs:", reqs)

		go func() {
			defer atomic.AddInt32(&max, -1)
			defer wg.Done()

			req := fasthttp.AcquireRequest()
			res := fasthttp.AcquireResponse()

			res.Reset()

			req.Header.SetMethod("GET")
			req.URI().Update("https://api.binance.com/api/v3/exchangeInfo")

			err := c.Do(req, res)
			if err != nil {
				log.Println(err)

				res.Header.VisitAll(func(k, v []byte) {
					fmt.Printf("%s: %s\n", k, v)
				})

				return
			}

			a := make(map[string]interface{})
			if err = json.Unmarshal(res.Body(), &a); err != nil {
				panic(err)
			}

			fmt.Println(len(res.Body()))
		}()
	}

	fmt.Printf("total reqs: %d\n", reqs)

	wg.Wait()
}
