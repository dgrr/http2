package http2

import (
	"net"
)

type Ctx struct {
	c        net.Conn
	streamID uint32
	hp       *HPACK

	Request  Request
	Response Response
}

func (ctx *Ctx) SetHPACK(hp *HPACK) {
	ctx.hp = hp
}

func (ctx *Ctx) SetStream(sid uint32) {
	ctx.streamID = sid
}
