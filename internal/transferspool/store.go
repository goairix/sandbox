// Package transferspool owns bounded, disposable HTTP download data.
package transferspool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	ErrCapacity    = errors.New("transfer capacity unavailable")
	ErrUnavailable = errors.New("transfer store unavailable")
)

type Options struct {
	Directory                   string
	MaxFileBytes, MaxTotalBytes int64
	MaxActive                   int
}

// Validate is deliberately pure: mounted configuration checks never open files.
func (o Options) Validate() error {
	if !filepath.IsAbs(o.Directory) || filepath.Clean(o.Directory) != o.Directory || strings.ContainsRune(o.Directory, 0) || o.MaxFileBytes < 1 || o.MaxFileBytes > 2<<30 || o.MaxTotalBytes < o.MaxFileBytes || o.MaxTotalBytes > 16<<30 || o.MaxActive < 1 || o.MaxActive > 8 {
		return ErrUnavailable
	}
	return nil
}

type Store struct {
	mu        sync.Mutex
	options   Options
	root      *os.File
	jobs      map[*Job]struct{}
	charged   int64
	closed    bool
	failure   error
	closeOnce sync.Once
	closeErr  error
}

func Open(options Options) (*Store, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	root, err := openDirectory(options.Directory)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := recoverStale(root); err != nil {
		return nil, errors.Join(ErrUnavailable, err, root.Close())
	}
	options.Directory = strings.Clone(options.Directory)
	return &Store{options: options, root: root, jobs: make(map[*Job]struct{})}, nil
}

func (s *Store) Reserve(ctx context.Context) (*Job, error) {
	if ctx == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed || s.failure != nil {
		s.mu.Unlock()
		return nil, ErrUnavailable
	}
	if len(s.jobs) >= s.options.MaxActive || s.charged > s.options.MaxTotalBytes-s.options.MaxFileBytes {
		s.mu.Unlock()
		return nil, ErrCapacity
	}
	owned, cancel := context.WithCancel(ctx)
	j := &Job{store: s, ctx: owned, cancel: cancel, done: make(chan struct{})}
	s.jobs[j] = struct{}{}
	s.charged += s.options.MaxFileBytes
	// Hold admission through creation so shutdown cannot close the directory FD.
	var random [16]byte
	_, err := rand.Read(random[:])
	if err == nil {
		j.name = "http-download-" + hex.EncodeToString(random[:])
		j.file, j.identity, err = createFile(s.root, j.name)
	}
	if err != nil {
		s.failure = errors.Join(s.failure, err)
		j.failure = err
	}
	s.mu.Unlock()
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err, j.Close())
	}
	if err := owned.Err(); err != nil {
		return nil, errors.Join(err, j.Close())
	}
	return j, nil
}

func (s *Store) latch(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.failure = errors.Join(s.failure, err)
	s.mu.Unlock()
}

func (s *Store) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failure != nil {
		return errors.Join(ErrUnavailable, s.failure)
	}
	return nil
}

// Close cancels admission and waits for owner releases. A context deadline or
// cancellation does not authorize closing an FD still owned by original IO.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		jobs := make([]*Job, 0, len(s.jobs))
		for j := range s.jobs {
			j.cancel()
			jobs = append(jobs, j)
		}
		s.mu.Unlock()
		for _, j := range jobs {
			<-j.done
		}
		s.mu.Lock()
		s.closeErr = s.failure
		s.mu.Unlock()
		s.closeErr = errors.Join(s.closeErr, s.root.Close())
	})
	return s.closeErr
}
