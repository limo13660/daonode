package rate

import (
	"net"
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
