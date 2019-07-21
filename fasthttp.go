// +build fasthttp

package http2

import (
	"github.com/valyala/fasthttp"
)

func translateFromCtx(ctx *Ctx) *fasthttp.RequestCtx {
	req := fasthttp.AcquireRequest()
	res := fasthttp.AcquireResponse()
	rctx := &fasthttp.RequestCtx{}
	req.CopyTo(&rctx.Request)
	res.CopyTo(&rctx.Response)
	fasthttp.ReleaseRequest(req)
	fasthttp.ReleaseResponse(res)

	for _, hf := range ctx.Request.Header.h {
		rctx.Request.Header.AddBytesKV(hf.key, hf.value)
	}
	rctx.Request.SetRequestURIBytes(
		ctx.Request.Header.path,
	)
	rctx.Request.Header.SetMethodBytes(
		ctx.Request.Header.method,
	)
	rctx.Request.Header.SetUserAgentBytes(
		ctx.Request.Header.userAgent,
	)

	return rctx
}

func translateToCtx(ctx *Ctx, rctx *fasthttp.RequestCtx) {
	rctx.Response.Header.VisitAll(func(k, v []byte) {
		ctx.Response.Header.AddBytes(k, v)
	})
	if len(rctx.Response.Body()) > 0 {
		ctx.Response.Write(rctx.Response.Body())
	}
}
