package filev2

import (
	"errors"
	"io"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileV2ExitStatus(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{err: unix.ENOENT, code: 2},
		{err: unix.EXDEV, code: 3},
		{err: unix.EACCES, code: 3},
		{err: ErrTypeMismatch, code: 4},
		{err: io.ErrUnexpectedEOF, code: 5},
		{err: errors.New("unknown"), code: 1},
	} {
		if got := ExitStatus(tc.err); got != tc.code {
			t.Errorf("ExitStatus(%v) = %d, want %d", tc.err, got, tc.code)
		}
	}
}
