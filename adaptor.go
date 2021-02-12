package http2

import (
	"github.com/valyala/fasthttp"
)

func fasthttpRequestHeaders(hp *HPACK, req *fasthttp.Request) {
	hp.Range(func(hf *HeaderField) {
		k, v := hf.KeyBytes(), hf.ValueBytes()
		if !hf.IsPseudo() { // TODO: && !bytes.Equal(k, strUserAgent) {
			req.Header.AddBytesKV(k, v)
			return
		}

		uri := req.URI()
		switch k[1] {
		case 'm': // method
			req.Header.SetMethodBytes(v)
		case 'p': // path
			uri.SetPathBytes(v)
		case 's': // scheme
			uri.SetSchemeBytes(v)
		case 'a': // authority
			uri.SetHostBytes(v)
		// TODO: See below?? case 'u': // user-agent
		// 	req.Header.SetUserAgentBytes(v)
		}
	})
}
