package runtime

import (
	"context"
	"errors"
	"io"
	"sync"
)

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
