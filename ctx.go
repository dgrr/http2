package http2

import (
	"net"
)

type Ctx struct {
	c   net.Conn
	sid uint32
	hp  *HPACK

	Request  Request
	Response Response
}

func (ctx *Ctx) SetHPACK(hp *HPACK) {
	ctx.hp = hp
	ctx.Request.Header.hp = hp
	ctx.Response.Header.hp = hp
}

func (ctx *Ctx) SetStream(sid uint32) {
	ctx.sid = sid
}

// writeFrame writes fr into the ctx's conn ...
func (ctx *Ctx) writeResponse() error {
	err := ctx.writeHeader(ctx.Response.b.Len() == 0)
	if err == nil && ctx.Response.b.Len() > 0 {
		err = ctx.writeBody(true)
	}

	return err
}

func (ctx *Ctx) writeHeader(endStream bool) error {
	ctx.Response.Header.parse()

	h := AcquireHeaders()
	fr := AcquireFrame()
	defer ReleaseHeaders(h)
	defer ReleaseFrame(fr)

	fr.SetStream(ctx.sid)

	h.SetEndStream(endStream)
	h.SetEndHeaders(true) // TODO: Check user settings ...
	h.SetPadding(true)
	h.SetHeaders(
		ctx.Response.Header.raw, // TODO: change response.header ...
	)
	h.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.c)
	return err
}

func (ctx *Ctx) writeBody(endStream bool) error {
	data := AcquireData()
	fr := AcquireFrame()
	defer ReleaseData(data)
	defer ReleaseFrame(fr)

	fr.SetStream(ctx.sid)
	data.SetPadding(true)
	data.SetEndStream(endStream)
	data.SetData(
		ctx.Response.Body(),
	)
	data.WriteFrame(fr)

	_, err := fr.WriteTo(ctx.c)
	return err
}
