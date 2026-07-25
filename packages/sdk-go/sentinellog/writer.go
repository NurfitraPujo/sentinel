package sentinellog

import (
	"errors"
	"io"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

type Writer struct {
	out io.Writer
}

func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out}
}

func (w *Writer) Write(p []byte) (n int, err error) {
	msg := string(p)
	sentinel.CaptureError(errors.New(msg), map[string]interface{}{
		"logger": "std_log",
	})

	if w.out != nil {
		return w.out.Write(p)
	}
	return len(p), nil
}
