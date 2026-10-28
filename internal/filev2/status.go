package filev2

import (
	"errors"
	"io"

	"golang.org/x/sys/unix"
)

var ErrTypeMismatch = errors.New("file is not regular")
var ErrInvalidWriteSize = errors.New("write body exceeds declared size")

const (
	ExitUnknown      = 1
	ExitMissing      = 2
	ExitForbidden    = 3
	ExitTypeMismatch = 4
	ExitInvalidSize  = 5
	ExitInvalidText  = 6
	ExitEditConflict = 7
	ExitTextLimit    = 8
)

// ExitStatus is the helper's bounded error protocol. Transports inspect only
// this numeric exit status; arbitrary stderr is never used as authorization
// or returned to public callers.
func ExitStatus(err error) int {
	switch {
	case errors.Is(err, unix.ENOENT):
		return ExitMissing
	case errors.Is(err, unix.EXDEV), errors.Is(err, unix.ELOOP), errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return ExitForbidden
	case errors.Is(err, ErrTypeMismatch):
		return ExitTypeMismatch
	case errors.Is(err, ErrInvalidWriteSize), errors.Is(err, io.ErrUnexpectedEOF):
		return ExitInvalidSize
	case errors.Is(err, ErrTextInvalid):
		return ExitInvalidText
	case errors.Is(err, ErrEditConflict):
		return ExitEditConflict
	case errors.Is(err, ErrTextLimit):
		return ExitTextLimit
	default:
		return ExitUnknown
	}
}
