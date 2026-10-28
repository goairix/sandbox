package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/goairix/sandbox/internal/filev2"
)

var ErrFilePathForbidden = errors.New("file path is forbidden")
var ErrFileTypeMismatch = errors.New("file type mismatch")
var ErrFileInvalidText = errors.New("invalid file text")
var ErrFileConflict = errors.New("file edit conflict")
var ErrFileLimitExceeded = errors.New("file limit exceeded")

// FileV2ExitError maps the helper's numeric status without parsing its stderr.
func FileV2ExitError(code int) error {
	switch code {
	case filev2.ExitMissing:
		return fmt.Errorf("%w: helper exit %d", ErrFileNotFound, code)
	case filev2.ExitForbidden:
		return fmt.Errorf("%w: helper exit %d", ErrFilePathForbidden, code)
	case filev2.ExitTypeMismatch:
		return fmt.Errorf("%w: helper exit %d", ErrFileTypeMismatch, code)
	case filev2.ExitInvalidSize:
		return fmt.Errorf("%w: helper exit %d", ErrInvalidUploadSize, code)
	case filev2.ExitInvalidText:
		return fmt.Errorf("%w: helper exit %d", ErrFileInvalidText, code)
	case filev2.ExitEditConflict:
		return fmt.Errorf("%w: helper exit %d", ErrFileConflict, code)
	case filev2.ExitTextLimit:
		return fmt.Errorf("%w: helper exit %d", ErrFileLimitExceeded, code)
	default:
		return fmt.Errorf("file helper exited with code %d", code)
	}
}

// FileV2Transport runs one sandbox-side helper process without shell-joining
// user fields. Close on the result must join that process before returning.
type FileV2Transport interface {
	RunFileOp(ctx context.Context, id string, argv []string, stdin io.Reader) (io.ReadCloser, error)
}

// FileListPage is a path-ordered directory page, not a directory snapshot.
type FileListPage struct {
	Files      []FileInfo `json:"files"`
	NextCursor string     `json:"next_cursor"`
}

type fileOpStream struct {
	reader   io.ReadCloser
	cancel   context.CancelFunc
	finished <-chan error
	once     sync.Once
	err      error
}

// NewFileOpStream binds an output reader to the helper's cancellation and
// completion. It is shared by Docker and Kubernetes transport adapters.
func NewFileOpStream(reader io.ReadCloser, cancel context.CancelFunc, finished <-chan error) io.ReadCloser {
	return &fileOpStream{reader: reader, cancel: cancel, finished: finished}
}

func (s *fileOpStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *fileOpStream) Close() error {
	s.once.Do(func() {
		// A completed stream publishes its result before closing its writer.
		// Preserve that result on a normal EOF instead of racing it with cancel.
		select {
		case result := <-s.finished:
			s.err = errors.Join(s.reader.Close(), result)
			s.cancel()
		default:
			s.cancel()
			s.err = errors.Join(s.reader.Close(), <-s.finished)
		}
	})
	return s.err
}
