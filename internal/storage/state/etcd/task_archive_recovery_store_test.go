package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func recoveryTestOptions(directory string, jobs uint64) TaskWorkspaceArchiveRecoveryOptions {
	l := archiveTestLimits()
	i, _ := taskArchiveIndexBytes(l.Entries)
	l.Jobs, l.TotalBytes = jobs, jobs*l.JobBytes
	l.JobIndexBytes = i + 4*taskArchiveRecoveryMetadataBytes
	l.TotalIndexBytes = jobs * l.JobIndexBytes
	return TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions{directory, l.Jobs, l.JobBytes, l.TotalBytes, l.Entries, l.JobIndexBytes, l.TotalIndexBytes}}
}

func TestTaskArchiveRecoveryRetention(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux persistent profile; host codec/limits remain exercised")
	}
	d := archiveTestDirectory(t)
	o := recoveryTestOptions(d, 2)
	s, err := newTaskArchiveRecoveryStore(o)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.reserveRecovery(context.Background(), recoveryTestSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(d, r.name)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); err != nil {
		t.Fatalf("persistent original archive missing after Close: %v", err)
	}
	cold, err := newTaskArchiveRecoveryStore(o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cold.close()) })
	diagnostic, err := cold.coldSnapshot(0)
	require.NoError(t, err)
	require.True(t, diagnostic.Occupied)
	require.True(t, diagnostic.Quarantined, "pending TAR never becomes received authority")
	require.Equal(t, uint64(1), cold.jobs)
	require.Equal(t, o.JobBytes, cold.disk)
	_, err = cold.reserveRecovery(context.Background(), recoveryTestSeed(t))
	require.NoError(t, err, "a separate confirmed empty slot remains admissible")
}

func TestTaskArchiveRecoveryStoreUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		require.NoError(t, taskArchiveRecoverySupported())
		return
	}
	require.Error(t, taskArchiveRecoverySupported())
	_, err := newTaskArchiveRecoveryStore(recoveryTestOptions(archiveTestDirectory(t), 1))
	require.Error(t, err)
}

func TestTaskArchiveRecoveryMetadataSessionExpiry(t *testing.T) {
	s := &taskArchiveStore{recovery: &taskArchiveRecoveryStore{gate: make(chan struct{}, 1)}}
	s.self = s
	s.recovery.gate <- struct{}{}
	session, stopSession := context.WithCancel(context.Background())
	stopSession()
	life, stopLife := context.WithCancel(context.Background())
	defer stopLife()
	r := &taskArchiveSpool{store: s, ctx: session, recovery: &taskArchiveRecoveryOwner{ctx: life, cancel: stopLife}}
	r.self = r
	bounded, done, err := r.beginRecoveryMetadata(context.Background())
	require.NoError(t, err, "original consumer registration survives send/session expiry")
	require.NoError(t, bounded.Err())
	unlock, err := r.recoveryGate(bounded)
	require.NoError(t, err)
	unlock()
	done()
	stopLife()
	_, _, err = r.beginRecoveryMetadata(context.Background())
	require.Error(t, err, "spool Close cancellation still rejects new metadata owners")
}

func TestTaskArchiveRecoveryStoreFullAndPeakQuota(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux descriptor store")
	}
	o := recoveryTestOptions(archiveTestDirectory(t), 64)
	s, err := newTaskArchiveRecoveryStore(o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.close()) })
	for i := 0; i < 64; i++ {
		r, err := s.reserveRecovery(context.Background(), recoveryTestSeed(t))
		require.NoError(t, err)
		require.EqualValues(t, i, r.recovery.slot)
		require.NoError(t, r.close(), "retirement cannot refund durable slot charge")
	}
	_, err = s.reserveRecovery(context.Background(), recoveryTestSeed(t))
	require.Error(t, err)
	require.Equal(t, uint64(64), s.jobs)
	require.Equal(t, o.TotalBytes, s.disk)
	require.Equal(t, o.TotalIndexBytes, s.memory)
	require.Empty(t, s.spools, "live owner map contains only original owners")
	for _, dimension := range []string{"disk", "memory"} {
		t.Run(dimension, func(t *testing.T) {
			limited := recoveryTestOptions(archiveTestDirectory(t), 2)
			if dimension == "disk" {
				limited.TotalBytes = limited.JobBytes
			} else {
				limited.TotalIndexBytes = limited.JobIndexBytes
			}
			store, err := newTaskArchiveRecoveryStore(limited)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.close()) })
			_, err = store.reserveRecovery(context.Background(), recoveryTestSeed(t))
			require.NoError(t, err)
			_, err = store.reserveRecovery(context.Background(), recoveryTestSeed(t))
			require.Error(t, err)
			_, err = os.Stat(filepath.Join(limited.Directory, "slot-01.json"))
			require.True(t, os.IsNotExist(err), "quota fails before filesystem creation")
		})
	}
}

// A structural store fixture, never a transported authority or cold source.
func recoveryStoreReceivedFixture(t *testing.T, s *taskArchiveStore) *taskArchiveSpool {
	t.Helper()
	r, err := s.reserveRecovery(context.Background(), recoveryTestSeed(t))
	require.NoError(t, err)
	data := archiveValidTar(t)
	_, err = r.file.Write(data)
	require.NoError(t, err)
	entries, err := taskArchiveValidateTar(context.Background(), r.file, uint64(len(data)), s.limits.Entries, r.index)
	require.NoError(t, err)
	h := sha256.Sum256(data)
	w := &taskReceivedArchive{origin: r, diagnostics: taskArchiveDiagnostics{Bytes: uint64(len(data)), Entries: entries, SHA256: hex.EncodeToString(h[:]), TicketDigest: r.recovery.manifest.Seed.TicketDigest}}
	w.self = w
	r.started, r.witness = true, w
	require.NoError(t, r.sealOriginalReceived(context.Background(), w))
	return r
}

func TestTaskArchiveRecoveryStoreFilesystemQuarantine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux descriptor store")
	}
	for _, fault := range []string{"tar-byte", "tar-mode", "tar-hardlink", "tar-symlink", "tar-rename", "json-unknown", "json-null", "json-oversize", "next-leftover", "root-boot", "root-mount", "root-inode"} {
		t.Run(fault, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			s, err := newTaskArchiveRecoveryStore(o)
			require.NoError(t, err)
			r := recoveryStoreReceivedFixture(t, s)
			m := r.recovery.manifest
			require.NoError(t, s.close())
			tar, metadata := filepath.Join(o.Directory, "slot-00.tar"), filepath.Join(o.Directory, "slot-00.json")
			original, err := os.ReadFile(metadata)
			require.NoError(t, err)
			t.Logf("original manifest before %s replacement: %s", fault, original)
			switch fault {
			case "tar-byte":
				f, err := os.OpenFile(tar, os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = f.WriteAt([]byte{1}, 0)
				require.NoError(t, err)
				require.NoError(t, f.Close())
			case "tar-mode":
				require.NoError(t, os.Chmod(tar, 0644))
			case "tar-hardlink":
				require.NoError(t, os.Link(tar, filepath.Join(archiveTestDirectory(t), "link")))
			case "tar-symlink":
				require.NoError(t, os.Remove(tar))
				require.NoError(t, os.Symlink(filepath.Join(archiveTestDirectory(t), "missing"), tar))
			case "tar-rename":
				require.NoError(t, os.Rename(tar, filepath.Join(archiveTestDirectory(t), "moved")))
			case "json-unknown":
				require.NoError(t, os.WriteFile(metadata, []byte("{\"unknown\":1}"), 0600))
			case "json-null":
				require.NoError(t, os.WriteFile(metadata, []byte("null"), 0600))
			case "json-oversize":
				require.NoError(t, os.WriteFile(metadata, make([]byte, taskArchiveRecoveryMetadataBytes+1), 0600))
			case "next-leftover":
				require.NoError(t, os.WriteFile(filepath.Join(o.Directory, "slot-00.next"), original, 0600))
			default:
				if fault == "root-boot" {
					m.Root.BootID = "22222222-2222-4222-8222-222222222222"
				} else if fault == "root-mount" {
					m.Root.MountID++
				} else {
					m.Root.Ino++
				}
				var out [taskArchiveRecoveryMetadataBytes]byte
				wire, err := encodeTaskArchiveRecovery(m, out[:])
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(metadata, wire, 0600))
			}
			cold, err := newTaskArchiveRecoveryStore(o)
			if fault == "json-oversize" {
				if cold != nil {
					require.NoError(t, cold.close())
				}
				require.Error(t, err, "oversized content must reject all admission")
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cold.close()) })
			d, err := cold.coldSnapshot(0)
			require.NoError(t, err)
			require.True(t, d.Occupied)
			require.True(t, d.Quarantined)
			require.Equal(t, o.JobBytes, cold.disk)
			_, err = cold.reserveRecovery(context.Background(), recoveryTestSeed(t))
			require.NoError(t, err, "other empty slot remains available")
		})
	}
	for _, name := range []string{"unknown", "slot-02.tar", "slot-64.json", "slot-0.tar", "slot-00.other"} {
		t.Run("reject-name-"+name, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			require.NoError(t, os.WriteFile(filepath.Join(o.Directory, name), []byte("preserve"), 0600))
			_, err := newTaskArchiveRecoveryStore(o)
			require.Error(t, err)
			preserved, err := os.ReadFile(filepath.Join(o.Directory, name))
			require.NoError(t, err)
			require.Equal(t, "preserve", string(preserved))
		})
	}
	for _, extension := range []string{"tar", "json", "next"} {
		t.Run("orphan-"+extension, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			require.NoError(t, os.WriteFile(filepath.Join(o.Directory, "slot-00."+extension), []byte("orphan"), 0600))
			s, err := newTaskArchiveRecoveryStore(o)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.close()) })
			d, err := s.coldSnapshot(0)
			require.NoError(t, err)
			require.True(t, d.Quarantined)
			require.Equal(t, o.JobBytes, s.disk)
		})
	}
	for _, fault := range []string{"config", "root-mode", "root-symlink", "root-live-replacement"} {
		t.Run(fault, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			s, err := newTaskArchiveRecoveryStore(o)
			require.NoError(t, err)
			_, err = s.reserveRecovery(context.Background(), recoveryTestSeed(t))
			require.NoError(t, err)
			if fault == "root-live-replacement" {
				require.NoError(t, os.Rename(o.Directory, o.Directory+"-original"))
				require.NoError(t, os.Mkdir(o.Directory, 0700))
				_, err = s.reserveRecovery(context.Background(), recoveryTestSeed(t))
				require.Error(t, err)
				require.NoError(t, s.close())
				return
			}
			require.NoError(t, s.close())
			switch fault {
			case "config":
				o.TotalBytes++
			case "root-mode":
				require.NoError(t, os.Chmod(o.Directory, 0755))
			case "root-symlink":
				link := filepath.Join(archiveTestDirectory(t), "root-link")
				require.NoError(t, os.Symlink(o.Directory, link))
				o.Directory = link
			}
			_, err = newTaskArchiveRecoveryStore(o)
			require.Error(t, err, fmt.Sprint(fault))
		})
	}
}

func TestTaskArchiveRecoveryColdContentQuota(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux descriptor store")
	}
	for _, extension := range []string{"tar", "json", "next"} {
		t.Run(extension, func(t *testing.T) {
			o := recoveryTestOptions(archiveTestDirectory(t), 2)
			limit := uint64(taskArchiveRecoveryMetadataBytes)
			if extension == "tar" {
				limit = o.JobBytes - 2*taskArchiveRecoveryMetadataBytes
			}
			path := filepath.Join(o.Directory, "slot-00."+extension)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			require.NoError(t, err)
			require.NoError(t, f.Truncate(int64(limit+1)))
			require.NoError(t, f.Close())
			cold, err := newTaskArchiveRecoveryStore(o)
			if cold != nil {
				require.NoError(t, cold.close())
			}
			require.Error(t, err, "oversized orphan must reject empty-slot admission")
			st, err := os.Lstat(path)
			require.NoError(t, err)
			require.Equal(t, int64(limit+1), st.Size(), "rejection preserves content")
		})
	}
}

func TestTaskArchiveRecoveryUnpublishedInitializerJoin(t *testing.T) {
	// Host lifecycle check only: the original registered FD/ownerDone is real;
	// no manifest or received authority is reconstructed by this fixture.
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	life, cancel := context.WithCancel(context.Background())
	r.recovery = &taskArchiveRecoveryOwner{ctx: life, cancel: cancel}
	s.mu.Lock()
	r.active = true
	r.ownerDone = make(chan struct{})
	s.mu.Unlock()
	b := &Backend{archiveStore: s, archiveLife: &taskArchiveBackend{}}
	closed := make(chan error, 1)
	go func() { closed <- b.Close() }()
	select {
	case <-r.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel unpublished store initializer")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close passed physical initializer ownerDone: %v", err)
	default:
	}
	s.mu.Lock()
	r.active = false
	close(r.ownerDone)
	s.mu.Unlock()
	require.NoError(t, <-closed)
	require.Error(t, life.Err())
	_, err = s.reserve(context.Background())
	require.Error(t, err, "closed store cannot admit an initializer after backend seal")
	require.EqualValues(t, 1, s.jobs, "recovery owner is retained without refund")
}

func TestTaskArchiveRecoveryAttemptExclusion(t *testing.T) {
	claim := &TaskClaim{}
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			claim.mu.Lock()
			if claim.sealArchiveRecoveryAttempt() {
				accepted.Add(1)
			}
			claim.mu.Unlock()
		}()
	}
	workers.Wait()
	require.EqualValues(t, 1, accepted.Load(), "one original initializer excludes competing attempts")
	claim.mu.Lock()
	defer claim.mu.Unlock()
	claim.finishArchiveRecoveryInitialization(&taskArchiveSpool{}, errTaskArchive)
	require.False(t, claim.sealArchiveRecoveryAttempt(), "charged failed attempt stays sealed")
	retry := &TaskClaim{}
	retry.mu.Lock()
	defer retry.mu.Unlock()
	require.True(t, retry.sealArchiveRecoveryAttempt())
	retry.finishArchiveRecoveryInitialization(nil, errTaskArchive)
	require.True(t, retry.sealArchiveRecoveryAttempt(), "definite precharge refusal can be retried")
	retry.finishArchiveRecoveryInitialization(&taskArchiveSpool{}, nil)
	require.False(t, retry.sealArchiveRecoveryAttempt(), "successful charged attempt stays sealed")
}
