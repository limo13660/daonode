package sudoku

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type captureNetConn struct{ bytes.Buffer }

func (c *captureNetConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *captureNetConn) Close() error                     { return nil }
func (c *captureNetConn) LocalAddr() net.Addr              { return nil }
func (c *captureNetConn) RemoteAddr() net.Addr             { return nil }
func (c *captureNetConn) SetDeadline(time.Time) error      { return nil }
func (c *captureNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *captureNetConn) SetWriteDeadline(time.Time) error { return nil }

func TestConnLargeWriteKeepsBoundedEncoderBuffer(t *testing.T) {
	raw := new(captureNetConn)
	table := NewTable("memory-bound-test", "entropy")
	conn := NewConn(raw, table, 0, 0, false)
	payload := bytes.Repeat([]byte("sudoku-large-stream-"), 512*1024)
	if n, err := conn.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("Write() = %d, %v; want %d, nil", n, err, len(payload))
	}
	if got := cap(conn.writeBuf); got > maxEncodedWriteSize {
		t.Fatalf("encoder retained %d bytes; want <= %d", got, maxEncodedWriteSize)
	}
	if raw.Len() == 0 {
		t.Fatal("large write produced no wire data")
	}
}
