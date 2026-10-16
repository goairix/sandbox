package transferspool

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

type Job struct {
	mu             sync.Mutex
	store          *Store
	ctx            context.Context
	cancel         context.CancelFunc
	file           *os.File
	name           string
	identity       fileIdentity
	written        int64
	sealed, closed bool
	failure        error
	done           chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

func (j *Job) Context() context.Context { return j.ctx }

func (j *Job) fail(err error) error {
	j.failure = errors.Join(j.failure, err)
	j.cancel()
	j.store.latch(err)
	return err
}

func (j *Job) Write(data []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.sealed || j.failure != nil {
		return 0, ErrUnavailable
	}
	if err := j.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > j.store.options.MaxFileBytes-j.written {
		return 0, j.fail(ErrCapacity)
	}
	written := 0
	for len(data) > 0 {
		if err := j.ctx.Err(); err != nil {
			return written, err
		}
		next := len(data)
		if next > 32<<10 {
			next = 32 << 10
		}
		n, err := j.file.Write(data[:next])
		written += n
		j.written += int64(n)
		if err == nil && n != next {
			err = io.ErrShortWrite
		}
		if err != nil {
			return written, j.fail(err)
		}
		data = data[n:]
	}
	return written, nil
}

func (j *Job) Seal() (int64, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed || j.sealed || j.failure != nil {
		return 0, ErrUnavailable
	}
	if err := j.ctx.Err(); err != nil {
		return 0, err
	}
	if err := j.file.Sync(); err != nil {
		return 0, j.fail(err)
	}
	if err := checkFile(j.store.root, j.name, j.file, j.identity); err != nil {
		return 0, j.fail(err)
	}
	info, err := j.file.Stat()
	if err == nil && info.Size() != j.written {
		err = ErrUnavailable
	}
	if err == nil {
		_, err = j.file.Seek(0, io.SeekStart)
	}
	if err != nil {
		return 0, j.fail(err)
	}
	j.sealed = true
	return j.written, nil
}

func (j *Job) Read(data []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.sealed || j.closed || j.failure != nil {
		return 0, ErrUnavailable
	}
	if err := j.ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) > 32<<10 {
		data = data[:32<<10]
	}
	n, err := j.file.Read(data)
	if err != nil && err != io.EOF {
		return n, j.fail(err)
	}
	return n, err
}

// Close is the original owner's release. The caller must physically join all
// writers and finish all readers before calling it; Store.Close only cancels.
func (j *Job) Close() error {
	j.closeOnce.Do(func() {
		j.cancel()
		j.mu.Lock()
		j.closed = true
		cleanupErr := removeFile(j.store.root, j.name, j.file, j.identity)
		j.closeErr = errors.Join(j.failure, cleanupErr)
		j.mu.Unlock()
		j.store.mu.Lock()
		if j.closeErr == nil {
			delete(j.store.jobs, j)
			j.store.charged -= j.store.options.MaxFileBytes
		} else {
			// Retain charge and bounded bookkeeping after any uncertain result.
			j.store.failure = errors.Join(j.store.failure, j.closeErr)
		}
		close(j.done)
		j.store.mu.Unlock()
	})
	return j.closeErr
}
