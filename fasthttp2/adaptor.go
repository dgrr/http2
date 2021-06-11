package fasthttp2

import (
	"bytes"
	"strconv"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

func fasthttpRequestHeaders(hf *http2.HeaderField, req *fasthttp.Request) {
	k, v := hf.KeyBytes(), hf.ValueBytes()
	if !hf.IsPseudo() &&
		!(bytes.Equal(k, http2.StringUserAgent) ||
			bytes.Equal(k, http2.StringContentType)) {
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
		req.URI().SetPathBytes(v)
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
}

func fasthttpResponseHeaders(dst *http2.Headers, hp *http2.HPACK, res *fasthttp.Response) {
	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

	hf.SetKeyBytes(http2.StringStatus)
	hf.SetValue(
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)
	dst.rawHeaders = hp.AppendHeader(dst.rawHeaders, hf, true)

	res.Header.SetContentLength(len(res.Body()))
	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(http2.toLower(k), v)
		dst.rawHeaders = hp.AppendHeader(dst.rawHeaders, hf, false)
	})
}
