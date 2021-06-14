package fasthttp2

import (
	"bytes"
	"strconv"

	"github.com/dgrr/http2"
	"github.com/valyala/fasthttp"
)

type ClientAdaptor struct {
	req *fasthttp.Request
	res *fasthttp.Response
	ch  chan error
}

func (adpr *ClientAdaptor) Error(err error) {
	adpr.ch <- err
}

func (adpr *ClientAdaptor) Close() {
	close(adpr.ch)
}

func (adpr *ClientAdaptor) Write(id uint32, enc *http2.HPACK, writer chan<- *http2.FrameHeader) {
	req := adpr.req

	hasBody := len(req.Body()) != 0

	fr := http2.AcquireFrameHeader()
	fr.SetStream(id)

	h := http2.AcquireFrame(http2.FrameHeaders).(*http2.Headers)
	fr.SetBody(h)

	hf := http2.AcquireHeaderField()

	hf.SetBytes(http2.StringAuthority, req.URI().Host())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringMethod, req.Header.Method())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringPath, req.URI().RequestURI())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringScheme, req.URI().Scheme())
	enc.AppendHeaderField(h, hf, true)

	hf.SetBytes(http2.StringUserAgent, req.Header.UserAgent())
	enc.AppendHeaderField(h, hf, true)

	req.Header.VisitAll(func(k, v []byte) {
		if bytes.EqualFold(k, http2.StringUserAgent) {
			return
		}

		hf.SetBytes(http2.ToLower(k), v)
		enc.AppendHeaderField(h, hf, false)
	})

	h.SetPadding(false)
	h.SetEndStream(!hasBody)
	h.SetEndHeaders(true)

	writer <- fr

	if hasBody { // has body
		fr = http2.AcquireFrameHeader()
		fr.SetStream(id)

		data := http2.AcquireFrame(http2.FrameData).(*http2.Data)
		fr.SetBody(data)

		// TODO: max length
		data.SetEndStream(true)
		data.SetData(req.Body())

		writer <- fr
	}
}

func (adpr *ClientAdaptor) Read(fr *http2.FrameHeader, dec *http2.HPACK) error {
	var err error
	res := adpr.res

	h := fr.Body().(http2.FrameWithHeaders)
	b := h.Headers()

	hf := http2.AcquireHeaderField()
	defer http2.ReleaseHeaderField(hf)

	for len(b) > 0 {
		b, err = dec.Next(hf, b)
		if err != nil {
			return err
		}

		if hf.IsPseudo() {
			if hf.KeyBytes()[1] == 's' { // status
				n, err := strconv.ParseInt(hf.Value(), 10, 64)
				if err != nil {
					return err
				}

				res.SetStatusCode(int(n))
				continue
			}
		}

		if bytes.Equal(hf.KeyBytes(), http2.StringContentLength) {
			n, _ := strconv.Atoi(hf.Value())
			res.Header.SetContentLength(n)
		} else {
			res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
		}
	}

	return nil
}

func (adpr *ClientAdaptor) AppendBody(body []byte) {
	adpr.res.AppendBody(body)
}
