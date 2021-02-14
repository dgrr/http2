package http2

import (
	"bytes"
	"github.com/valyala/fasthttp"
	"strconv"
)

func fasthttpRequestHeaders(hp *HPACK, req *fasthttp.Request) {
	hp.Range(func(hf *HeaderField) {
		k, v := hf.KeyBytes(), hf.ValueBytes()
		if !hf.IsPseudo() &&
			!(bytes.Equal(k, strUserAgent) ||
				bytes.Equal(k, strContentType)) {
			req.Header.AddBytesKV(k, v)
			return
		}

		if hf.IsPseudo() {
			k = k[1:]
		}

		switch k[0] {
		case 'm': // method
			req.Header.SetMethodBytes(v)
		case 'p': // path
			req.SetRequestURIBytes(v)
		case 's': // scheme
			req.URI().SetSchemeBytes(v)
		case 'a': // authority
			req.URI().SetHostBytes(v)
			req.Header.AddBytesV("Host", v)
		case 'u': // user-agent
			req.Header.SetUserAgentBytes(v)
		case 'c': // content-type
			req.Header.SetContentTypeBytes(v)
		}
	})
}

func fasthttpResponseHeaders(hp *HPACK, res *fasthttp.Response) {
	hp.AddBytesK(strStatus,
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)

	hp.AddBytesK(strContentLength,
		strconv.FormatInt(int64(len(res.Body())), 10),
	)

	res.Header.VisitAll(func(k, v []byte) {
		hp.AddBytes(bytes.ToLower(k), v)
	})
}
