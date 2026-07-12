package rate

import "net"

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

/*
type PacketConnCounter struct {
	network.PacketConn
	limiter *ratelimit.Bucket
}

func NewPacketConnCounter(conn network.PacketConn, l *ratelimit.Bucket) network.PacketConn {
	return &PacketConnCounter{
		PacketConn: conn,
		limiter:    l,
	}
}

func (p *PacketConnCounter) ReadPacket(buff *buf.Buffer) (destination M.Socksaddr, err error) {
	pLen := buff.Len()
	destination, err = p.PacketConn.ReadPacket(buff)
	p.limiter.Wait(int64(buff.Len() - pLen))
	return destination, err
}

func (p *PacketConnCounter) WritePacket(buff *buf.Buffer, destination M.Socksaddr) (err error) {
	p.limiter.Wait(int64(buff.Len()))
	return p.PacketConn.WritePacket(buff, destination)
}
*/
