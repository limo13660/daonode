package connutil

import "io"

// CloseReader is implemented by connections that support half-closing reads.
type CloseReader interface{ CloseRead() error }

// CloseWriter is implemented by connections that support half-closing writes.
type CloseWriter interface{ CloseWrite() error }

type closer interface{ Close() error }

func TryCloseRead(target any) error {
	if target == nil {
		return nil
	}
	if cr, ok := target.(CloseReader); ok {
		return cr.CloseRead()
	}
	return nil
}

func TryCloseWrite(target any) error {
	if target == nil {
		return nil
	}
	if cw, ok := target.(CloseWriter); ok {
		return cw.CloseWrite()
	}
	if c, ok := target.(closer); ok {
		return c.Close()
	}
	return nil
}

// WriteFull writes all bytes or returns the first write error.
func WriteFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Copy forwards data using an endpoint's optimized transfer method when one is available.
func Copy(dst io.Writer, src io.Reader) (int64, error) {
	if wt, ok := src.(io.WriterTo); ok {
		return wt.WriteTo(dst)
	}
	if rf, ok := dst.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(dst, src)
}
