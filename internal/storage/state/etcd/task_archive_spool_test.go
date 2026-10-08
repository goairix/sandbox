package etcd

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func archiveTestLimits() taskArchiveLimits {
	return taskArchiveLimits{Jobs: 2, JobBytes: 1 << 20, TotalBytes: 2 << 20, Entries: 32, JobIndexBytes: 64 * 33, TotalIndexBytes: 128 * 33}
}

func TestTaskArchiveSpoolAdmissionIdentityAndRefund(t *testing.T) {
	d := archiveTestDirectory(t)
	s, err := newTaskArchiveStore(d, archiveTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	if other, err := newTaskArchiveStore(d, archiveTestLimits()); err == nil || other != nil {
		t.Fatal("second original store reset quotas for the same private directory")
	}
	// Preserve the copied origin seal without copying a live mutex.
	copyStore := taskArchiveStore{self: s, root: s.root, limits: s.limits}
	if r, err := copyStore.reserve(context.Background()); err == nil || r != nil {
		t.Fatal("store copy admitted")
	}
	r1, err := s.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.reserve(context.Background()); err == nil || r != nil {
		t.Fatal("concurrency admission overflow")
	}
	if len(r1.index) != int(s.indexBytes) || s.disk != 2<<20 || s.memory != 128*33 {
		t.Fatal("exact quotas not held")
	}
	info, err := os.Stat(filepath.Join(d, r1.name))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file is not private")
	}
	copySpool := *r1
	if copySpool.close() == nil {
		t.Fatal("copy closed original")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r1.close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s.jobs != 1 || s.disk != 1<<20 || s.memory != 64*33 {
		t.Fatal("close refund not exact")
	}
	r3, err := s.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r3.close(); err != nil {
		t.Fatal(err)
	}
	if err := r2.close(); err != nil {
		t.Fatal(err)
	}
	if s.jobs != 0 || s.disk != 0 || s.memory != 0 {
		t.Fatal("quotas retained after original cleanup")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reserve(context.Background()); err == nil {
		t.Fatal("shutdown admitted")
	}
}

func TestTaskArchiveSpoolRejectsLimitsAndPrivateDirectory(t *testing.T) {
	for _, change := range []func(*taskArchiveLimits){
		func(l *taskArchiveLimits) { l.Jobs = 0 }, func(l *taskArchiveLimits) { l.JobBytes = 1535 },
		func(l *taskArchiveLimits) { l.JobBytes = 1<<40 + 1 }, func(l *taskArchiveLimits) { l.TotalBytes = l.JobBytes - 1 },
		func(l *taskArchiveLimits) { l.Entries = 0 }, func(l *taskArchiveLimits) { l.Entries = 1000001 },
		func(l *taskArchiveLimits) { l.JobIndexBytes-- }, func(l *taskArchiveLimits) { l.TotalIndexBytes = 0 },
	} {
		l := archiveTestLimits()
		change(&l)
		if s, err := newTaskArchiveStore(archiveTestDirectory(t), l); err == nil || s != nil {
			t.Fatal("invalid limits admitted")
		}
	}
	d := archiveTestDirectory(t)
	if err := os.Chmod(d, 0755); err != nil {
		t.Fatal(err)
	}
	if s, err := newTaskArchiveStore(d, archiveTestLimits()); err == nil || s != nil {
		t.Fatal("public directory accepted")
	}
	link := filepath.Join(archiveTestDirectory(t), "link")
	if err := os.Symlink(archiveTestDirectory(t), link); err != nil {
		t.Fatal(err)
	}
	if s, err := newTaskArchiveStore(link, archiveTestLimits()); err == nil || s != nil {
		t.Fatal("symlink root accepted")
	}
	for _, d := range []string{"relative", "/tmp/../tmp"} {
		if s, err := newTaskArchiveStore(d, archiveTestLimits()); err == nil || s != nil {
			t.Fatal("unclean root accepted")
		}
	}
	for _, field := range []string{"disk", "index"} {
		l := archiveTestLimits()
		if field == "disk" {
			l.TotalBytes = l.JobBytes
		} else {
			l.TotalIndexBytes = l.JobIndexBytes
		}
		s, err := newTaskArchiveStore(archiveTestDirectory(t), l)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.reserve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.reserve(context.Background()); err == nil {
			t.Fatal("global quota exceeded")
		}
		if err := r.close(); err != nil {
			t.Fatal(err)
		}
		if err := s.close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTaskArchiveSpoolUncertainIdentityPreservesAccounting(t *testing.T) {
	for _, fault := range []string{"hardlink", "symlink", "replacement", "mode"} {
		t.Run(fault, func(t *testing.T) {
			d := archiveTestDirectory(t)
			s, err := newTaskArchiveStore(d, archiveTestLimits())
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.reserve(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(d, r.name)
			switch fault {
			case "hardlink":
				err = os.Link(p, filepath.Join(d, "extra"))
			case "symlink", "replacement":
				err = os.Rename(p, filepath.Join(d, "original"))
				if err == nil {
					if fault == "symlink" {
						err = os.Symlink("original", p)
					} else {
						err = os.WriteFile(p, []byte("foreign"), 0600)
					}
				}
			case "mode":
				err = os.Chmod(p, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.close(); err == nil {
				t.Fatal("uncertain cleanup succeeded")
			}
			if _, err := os.Lstat(p); err != nil {
				t.Fatal("foreign or uncertain file removed")
			}
			if s.jobs != 1 || s.disk != s.limits.JobBytes || s.memory != s.indexBytes {
				t.Fatal("uncertain bytes refunded")
			}
			if s.close() == nil {
				t.Fatal("uncertain shutdown succeeded")
			}
		})
	}
}

func TestTaskArchiveSpoolCloseJoinsPhysicalOwner(t *testing.T) {
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.reserve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	r.active = true
	r.ownerDone = make(chan struct{})
	s.mu.Unlock()
	closed := make(chan error, 1)
	go func() { closed <- s.close() }()
	<-r.ctx.Done()
	select {
	case <-closed:
		t.Fatal("cancel refunded before physical owner joined")
	case <-time.After(20 * time.Millisecond):
	}
	s.mu.Lock()
	if s.disk != s.limits.JobBytes || r.file == nil {
		t.Fatal("live owner lost reservation")
	}
	r.active = false
	close(r.ownerDone)
	s.mu.Unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func archiveTestDirectory(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTaskArchiveSpoolRejectsColdFiles(t *testing.T) {
	d := archiveTestDirectory(t)
	p := filepath.Join(d, "cold")
	if err := os.WriteFile(p, []byte("retained unverified"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newTaskArchiveStore(d, archiveTestLimits())
	if err == nil || s != nil {
		t.Fatal("nonempty private directory accepted; cold bytes escaped admission")
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "retained unverified" {
		t.Fatal("cold bytes changed")
	}
}
