package rate

import (
	"net"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func NewConnRateLimiter(c net.Conn, l *DynamicBucket) *Conn {
	return &Conn{
		Conn:    c,
		limiter: l,
	}
}

type Conn struct {
	net.Conn
	limiter *DynamicBucket
}

func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	if n > 0 {
		c.limiter.Get().Wait(int64(n))
	}
	return n, err
}

func (c *Conn) Write(b []byte) (n int, err error) {
	c.limiter.Get().Wait(int64(len(b)))
	return c.Conn.Write(b)
}

type PacketConnRateLimiter struct {
	N.PacketConn
	limiter *DynamicBucket
}

func NewPacketConnRateLimiter(conn N.PacketConn, limiter *DynamicBucket) N.PacketConn {
	return &PacketConnRateLimiter{
		PacketConn: conn,
		limiter:    limiter,
	}
}

func (p *PacketConnRateLimiter) ReadPacket(buffer *buf.Buffer) (destination M.Socksaddr, err error) {
	initialLen := buffer.Len()
	destination, err = p.PacketConn.ReadPacket(buffer)
	if packetLen := buffer.Len() - initialLen; packetLen > 0 {
		p.limiter.Get().Wait(int64(packetLen))
	}
	return destination, err
}

func (p *PacketConnRateLimiter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	if packetLen := buffer.Len(); packetLen > 0 {
		p.limiter.Get().Wait(int64(packetLen))
	}
	return p.PacketConn.WritePacket(buffer, destination)
}

func (p *PacketConnRateLimiter) Upstream() any {
	return p.PacketConn
}
