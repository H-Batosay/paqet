package buffer

import (
	"io"
)

func CopyU(dst io.Writer, src io.Reader) error {
	buf := GetUBuf()
	defer PutUBuf(buf)
	_, err := io.CopyBuffer(dst, src, buf)
	return err
}
