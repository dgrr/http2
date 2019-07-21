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

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	// TODO: Check user settings ...
	fr.SetStream(ctx.sid)
	fr.SetType(FrameHeaders)
	if endStream {
		fr.Add(FlagEndStream)
	}
	fr.Add(FlagEndHeaders)

	/*
		fr.Add(FlagPadded)
		n := fastrand.Uint32n(256)
		nn := len(h.rawHeaders)
		h.rawHeaders = resize(h.rawHeaders, nn+n)
		h.rawHeaders = append(h.rawHeaders[:0], h.rawHeaders[1:]...)
		h.rawHeaders[0] = uint8(n)
		rand.Read(h.rawHeaders[nn+1 : nn+n])
	*/

	fr.SetPayload(
		ctx.Response.Header.raw,
	)
	_, err := fr.WriteTo(ctx.c)
	return err
}

func (ctx *Ctx) writeBody(endStream bool) error {
	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	fr.SetStream(ctx.sid)
	fr.SetType(FrameData)
	if endStream {
		fr.Add(FlagEndStream)
	}
	fr.SetPayload(
		ctx.Response.Body(),
	)
	_, err := fr.WriteTo(ctx.c)
	return err
}
