package main

import (
	"crypto/tls"
	"log"
	"os"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

func main() {
	d := http2.Dialer{
		Addr: "api.binance.com:443",
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}

	if c, err := d.Dial(); err != nil {
		log.Fatalln(err)
	} else {
		handleConn(c)
	}
}

func handleConn(c *http2.Conn) {
	defer c.Close()

	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()

	req.Header.SetMethod("GET")
	req.SetRequestURI("https://api.binance.com/api/v3/exchangeInfo")

	if id, err := c.Write(req); err != nil {
		log.Fatalln(err)
	} else {
		nid, err := c.Read(res)
		if err != nil {
			log.Fatalln(id, err)
		}

		if id != nid {
			log.Fatalf("Request and response id's doesn't match: %d<>%d\n", id, nid)
		}

		res.WriteTo(os.Stdout)
	}
}
