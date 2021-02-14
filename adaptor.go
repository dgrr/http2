package http2

import (
	"bytes"
	"github.com/valyala/fasthttp"
	"strconv"
)

func fasthttpRequestHeaders(hf *HeaderField, req *fasthttp.Request) {
	k, v := hf.KeyBytes(), hf.ValueBytes()
	if !hf.IsPseudo() &&
		!(bytes.Equal(k, strUserAgent) ||
			bytes.Equal(k, strContentType)) {
		req.Header.AddBytesKV(k, v)
		return
	}

	if hf.IsPseudo() {
		if bytes.Equal(k, strPath) {
			req.SetRequestURIBytes(v)
			return
		}

		k = k[1:]
	}

	switch k[0] {
	case 'm': // method
		req.Header.SetMethodBytes(v)
	case 'p': // path
		// req.URI().SetPathBytes(v)
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

func fasthttpResponseHeaders(dst *Headers, hp *HPACK, res *fasthttp.Response) {
	hf := AcquireHeaderField()
	defer ReleaseHeaderField(hf)

	hf.SetKeyBytes(strStatus)
	hf.SetValue(
		strconv.FormatInt(
			int64(res.Header.StatusCode()), 10,
		),
	)
	dst.rawHeaders = hp.AppendHeader(dst.rawHeaders, hf)

	hf.SetKeyBytes(strContentLength)
	hf.SetValue(
		strconv.FormatInt(int64(len(res.Body())), 10),
	)
	dst.rawHeaders = hp.AppendHeader(dst.rawHeaders, hf)

	res.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(bytes.ToLower(k), v)
		dst.rawHeaders = hp.AppendHeader(dst.rawHeaders, hf)
	})
}
