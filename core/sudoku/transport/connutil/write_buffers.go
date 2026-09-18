package connutil

import (
	"io"
	"net"
)

type BuffersWriter interface {
	WriteBuffers(net.Buffers) (int64, error)
}

// WriteBuffers preserves a writer's coalescing path where available and
// handles short writes without copying the caller's buffers.
func WriteBuffers(w io.Writer, buffers net.Buffers) (int64, error) {
	if writer, ok := w.(BuffersWriter); ok {
		n, err := writer.WriteBuffers(buffers)
		if err == nil {
			var want int64
			for _, p := range buffers {
				want += int64(len(p))
			}
			if n != want {
				err = io.ErrShortWrite
			}
		}
		return n, err
	}
	var total int64
	for _, p := range buffers {
		for len(p) > 0 {
			n, err := w.Write(p)
			if n < 0 || n > len(p) {
				return total, io.ErrShortWrite
			}
			total += int64(n)
			p = p[n:]
			if err != nil {
				return total, err
			}
			if n == 0 {
				return total, io.ErrShortWrite
			}
		}
	}
	return total, nil
}
