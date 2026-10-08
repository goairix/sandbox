package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"path/filepath"
)

const taskArchiveSlotBytes = uint64(33)

func taskArchiveIndexBytes(entries uint64) (uint64, error) {
	if entries == 0 || entries > 1000000 {
		return 0, errTaskArchive
	}
	slots := uint64(2)
	for slots < entries*2 {
		slots *= 2
	}
	return slots * taskArchiveSlotBytes, nil
}

// validateTaskArchiveStore has no filesystem or operator callback effects.
// Both optional Backend options and actual private store opening use it.
func validateTaskArchiveStore(directory string, l taskArchiveLimits) (uint64, error) {
	index, err := taskArchiveIndexBytes(l.Entries)
	if err != nil || l.Jobs == 0 || l.JobBytes < 1536 || l.JobBytes > 1<<40 || l.TotalBytes < l.JobBytes || l.JobIndexBytes < index || l.TotalIndexBytes < l.JobIndexBytes || index > uint64(math.MaxInt) || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return 0, errTaskArchive
	}
	return index, nil
}

func newTaskArchiveStore(directory string, l taskArchiveLimits) (*taskArchiveStore, error) {
	index, err := validateTaskArchiveStore(directory, l)
	if err != nil {
		return nil, err
	}
	root, err := taskArchiveOpenRoot(directory)
	if err != nil {
		return nil, err
	}
	s := &taskArchiveStore{root: root, limits: l, indexBytes: index, spools: make(map[*taskArchiveSpool]struct{})}
	s.self = s
	return s, nil
}

// Admission precedes file creation and any command or DATA IO. The only map
// stores admitted original owners, never arbitrary archive paths.
func (s *taskArchiveStore) reserve(ctx context.Context) (*taskArchiveSpool, error) {
	if s == nil || s.self != s || ctx == nil {
		return nil, errTaskArchive
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed || s.jobs >= s.limits.Jobs || s.disk > s.limits.TotalBytes-s.limits.JobBytes || s.memory > s.limits.TotalIndexBytes-s.indexBytes {
		s.mu.Unlock()
		return nil, errTaskArchive
	}
	owned, cancel := context.WithCancel(ctx)
	r := &taskArchiveSpool{store: s, ctx: owned, cancel: cancel, active: true, ownerDone: make(chan struct{})}
	r.self = r
	s.jobs++
	s.disk += s.limits.JobBytes
	s.memory += s.indexBytes
	s.spools[r] = struct{}{}
	s.mu.Unlock()
	var random [16]byte
	_, err := rand.Read(random[:])
	if err == nil {
		r.name = "archive-" + hex.EncodeToString(random[:])
		r.file, r.identity, err = taskArchiveCreate(s.root, r.name)
	}
	if err == nil {
		r.index = make([]byte, int(s.indexBytes))
		err = owned.Err()
	}
	s.mu.Lock()
	r.active = false
	close(r.ownerDone)
	s.mu.Unlock()
	if err != nil {
		return nil, errors.Join(err, r.close())
	}
	return r, nil
}

func (r *taskArchiveSpool) close() error {
	if r == nil || r.self != r || r.store == nil || r.store.self != r.store {
		return errTaskArchive
	}
	s := r.store
	s.mu.Lock()
	if r.closeDone != nil {
		done := r.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := r.closeErr
		s.mu.Unlock()
		return err
	}
	r.closing = true
	r.closeDone = make(chan struct{})
	done := r.ownerDone
	s.mu.Unlock()
	r.cancel()
	<-done // Cancellation alone never proves the physical owner returned.
	err := taskArchiveRemove(s.root, r.name, r.file, r.identity)
	s.mu.Lock()
	if err == nil {
		delete(s.spools, r)
		s.jobs--
		s.disk -= s.limits.JobBytes
		s.memory -= s.indexBytes
		r.index = nil
	}
	r.closeErr = err
	close(r.closeDone)
	s.mu.Unlock()
	return err
}

func (s *taskArchiveStore) close() error {
	if s == nil || s.self != s {
		return errTaskArchive
	}
	s.mu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	s.closeDone = make(chan struct{})
	owners := make([]*taskArchiveSpool, 0, len(s.spools))
	for r := range s.spools {
		owners = append(owners, r)
	}
	s.mu.Unlock()
	// Cancel all owners before joining any, without a lock across callbacks.
	for _, r := range owners {
		r.cancel()
	}
	var err error
	for _, r := range owners {
		err = errors.Join(err, r.close())
	}
	err = errors.Join(err, s.root.Close())
	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}
