package etcd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func syncSourceNativeFixture(t *testing.T, ttl time.Duration, exclude ...string) (*quiescenceNativeFixture, *ReceivedTaskWorkspaceArchive, TaskWorkspaceSyncPlan) {
	t.Helper()
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/"), Exclude: exclude, DeleteExtraneous: true})
	require.NoError(t, err)
	f, prepared, wire := deliveryNativeFixtureWithPlan(t, 0, func(record TaskRecord) TaskFinalSyncPlan {
		require.Equal(t, plan.workspace.Hash(), record.WorkspaceHash)
		p, err := plan.FinalSyncPlan()
		require.NoError(t, err)
		return p
	}, ttl)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	r := deliveryWait(t, done)
	require.NoError(t, r.err)
	require.NoError(t, <-wire.done)
	return f, r.archive, plan
}

func TestTaskWorkspaceSyncSourceNativeOriginalAdmission(t *testing.T) {
	f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.NoError(t, err)
	require.NotNil(t, source)
	entry, err := source.next(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "z", entry.path)
	require.Equal(t, byte(tar.TypeDir), entry.kind)
	emptyHash := sha256.Sum256(nil)
	require.Equal(t, hex.EncodeToString(emptyHash[:]), entry.sha256)
	entry, err = source.next(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "z/"+strings.Repeat("x", 120), entry.path)
	actual := make([]byte, entry.size)
	_, err = archive.owner.spool.file.ReadAt(actual, entry.offset)
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual), "PAX logical offset")
	copyEntry := *entry
	_, err = source.openBody(f.ctx, &copyEntry)
	require.Error(t, err)
	body, err := source.openBody(f.ctx, entry)
	require.NoError(t, err)
	_, err = source.openBody(f.ctx, entry)
	require.Error(t, err)
	actual, err = io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(actual))
	sum := sha256.Sum256(actual)
	require.Equal(t, hex.EncodeToString(sum[:]), entry.sha256)
	require.NoError(t, body.Close())
	entry, err = source.next(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "a", entry.path)
	body, err = source.openBody(f.ctx, entry)
	require.NoError(t, err)
	actual, err = io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "bye", string(actual))
	require.NoError(t, body.Close())
	_, err = source.next(f.ctx)
	require.ErrorIs(t, err, io.EOF)
	copySource := &PreparedTaskWorkspaceSyncSource{self: source.self, archive: source.archive}
	require.Error(t, copySource.Close())
	_, err = f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.Error(t, err)
	require.NoError(t, source.Close())
	require.NoError(t, source.Close())
}

func TestTaskWorkspaceSyncSourceNativeSkipsExcludedDirectory(t *testing.T) {
	f, archive, plan := syncSourceNativeFixture(t, 30*time.Second, "z")
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.NoError(t, err)
	entry, err := source.next(f.ctx)
	require.NoError(t, err)
	require.Equal(t, "a", entry.path)
	body, err := source.openBody(f.ctx, entry)
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "bye", string(data))
	require.NoError(t, body.Close())
	_, err = source.next(f.ctx)
	require.ErrorIs(t, err, io.EOF)
	require.NoError(t, source.Close())
}

func TestTaskWorkspaceSyncSourceNativeMismatchAndDefaultFixture(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
		wrong, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: plan.workspace, Exclude: []string{"z"}, DeleteExtraneous: true})
		require.NoError(t, err)
		copyArchive := *archive
		foreign := &Backend{archiveLife: f.b.archiveLife, archiveStore: f.b.archiveStore}
		for _, candidate := range []struct {
			b *Backend
			r *ReceivedTaskWorkspaceArchive
			p TaskWorkspaceSyncPlan
		}{{f.b, archive, wrong}, {f.b, &copyArchive, plan}, {foreign, archive, plan}} {
			_, err = candidate.b.PrepareTaskWorkspaceSyncSource(f.ctx, candidate.r, candidate.p)
			require.Error(t, err)
			require.Nil(t, archive.owner.spool.consumer)
		}
		source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
		require.NoError(t, err)
		require.NoError(t, source.Close())
	})
	t.Run("unchanged-default", func(t *testing.T) {
		f, prepared, wire := deliveryNativeFixture(t, 0)
		done := deliveryBegin(f, prepared)
		deliveryWaitSent(t, wire, done)
		close(wire.body)
		r := deliveryWait(t, done)
		require.NoError(t, r.err)
		require.NoError(t, <-wire.done)
		plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")})
		require.NoError(t, err)
		_, err = f.b.PrepareTaskWorkspaceSyncSource(f.ctx, r.archive, plan)
		require.Error(t, err)
		require.Nil(t, r.archive.owner.spool.consumer)
		require.NoError(t, r.archive.Close())
	})
}

func TestTaskWorkspaceSyncSourceNativeTamperAndPartial(t *testing.T) {
	for _, fault := range []string{"hash", "size", "replacement", "partial", "body-tamper"} {
		t.Run(fault, func(t *testing.T) {
			f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
			r := archive.owner.spool
			if fault == "hash" {
				_, err := r.file.WriteAt([]byte("changed"), 0)
				require.NoError(t, err)
			}
			if fault == "size" {
				require.NoError(t, r.file.Truncate(int64(archive.witness.diagnostics.Bytes)-512))
			}
			if fault == "replacement" {
				path := r.store.root.Name() + "/" + r.name
				require.NoError(t, os.Rename(path, path+"-original"))
				file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				require.NoError(t, err)
				require.NoError(t, file.Close())
			}
			source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
			if fault == "hash" || fault == "size" || fault == "replacement" {
				require.Error(t, err)
				require.Nil(t, source)
				if fault == "replacement" {
					require.EqualValues(t, 1, r.store.jobs, "uncertain identity retains quota")
					require.Error(t, f.b.Close())
				}
				return
			}
			require.NoError(t, err)
			_, err = source.next(f.ctx)
			require.NoError(t, err)
			entry, err := source.next(f.ctx)
			require.NoError(t, err)
			body, err := source.openBody(f.ctx, entry)
			require.NoError(t, err)
			if fault == "body-tamper" {
				_, err = r.file.WriteAt([]byte("j"), entry.offset)
				require.NoError(t, err)
			}
			if fault == "partial" {
				var data [1]byte
				_, err = body.Read(data[:])
				require.NoError(t, err)
				require.Error(t, body.Close())
			} else {
				_, err = io.ReadAll(body)
				require.Error(t, err)
			}
			_, err = source.next(f.ctx)
			require.Error(t, err)
			require.EqualValues(t, 1, r.store.jobs, "body Close/cancellation alone retains quota")
			require.NoError(t, source.Close())
		})
	}
}

// The seam holds completion after original File.ReadAt returns. It demonstrates
// registered physical owner joining, not a blocked kernel syscall.
func syncSourceHold(t *testing.T, s *PreparedTaskWorkspaceSyncSource) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.mu.Lock()
	s.readComplete = func() { once.Do(func() { close(entered) }); <-release }
	s.mu.Unlock()
	return entered, func() { close(release) }
}

func TestTaskWorkspaceSyncSourceNativeCloseJoinsOriginalRead(t *testing.T) {
	for _, route := range []string{"source", "artifact", "store", "backend", "release", "failed-renew", "body"} {
		t.Run(route, func(t *testing.T) {
			f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
			source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
			require.NoError(t, err)
			_, err = source.next(f.ctx)
			require.NoError(t, err)
			entry, err := source.next(f.ctx)
			require.NoError(t, err)
			body, err := source.openBody(f.ctx, entry)
			require.NoError(t, err)
			entered, release := syncSourceHold(t, source)
			readDone := make(chan error, 1)
			go func() { var data [1]byte; _, err := body.Read(data[:]); readDone <- err }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("original read did not reach completion seam")
			}
			closed := make(chan error, 1)
			go func() {
				var err error
				switch route {
				case "source":
					err = source.Close()
				case "artifact":
					err = archive.Close()
				case "store":
					err = f.b.archiveStore.close()
				case "backend":
					err = f.b.Close()
				case "release":
					err = f.b.ReleaseTaskClaim(f.ctx, f.c)
				case "failed-renew":
					_, err = f.raw.Delete(f.ctx, f.c.guardKey)
					if err == nil {
						err = f.b.RenewTaskClaim(f.ctx, f.c)
					}
				case "body":
					err = body.Close()
				}
				closed <- err
			}()
			var concurrentClose chan error
			if route == "backend" {
				concurrentClose = make(chan error, 1)
				go func() { concurrentClose <- f.b.Close() }()
			}
			if route != "body" {
				select {
				case <-source.ctx.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("close did not cancel source")
				}
			}
			select {
			case <-closed:
				t.Fatal("close returned before registered read completion")
			case <-time.After(30 * time.Millisecond):
			}
			if concurrentClose != nil {
				select {
				case <-concurrentClose:
					t.Fatal("concurrent Close returned before physical join")
				default:
				}
			}
			r := archive.owner.spool
			r.store.mu.Lock()
			require.EqualValues(t, 1, r.store.jobs)
			require.NotEmpty(t, r.index)
			r.store.mu.Unlock()
			_, err = r.file.Stat()
			require.NoError(t, err, "original file remains open")
			select {
			case <-f.b.client.Ctx().Done():
				t.Fatal("native client closed before physical join")
			default:
			}
			release()
			<-readDone
			err = <-closed
			if concurrentClose != nil {
				require.NoError(t, <-concurrentClose)
			}
			if route == "failed-renew" || route == "body" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if route == "body" {
				require.EqualValues(t, 1, r.store.jobs)
				require.NoError(t, source.Close())
			}
			require.Zero(t, r.store.jobs)
		})
	}
}

func TestTaskWorkspaceSyncSourceNativeCurrentFences(t *testing.T) {
	for _, fault := range []string{"restore", "command-aba", "operation", "local-loss", "failed-query"} {
		t.Run(fault, func(t *testing.T) {
			f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
			source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
			require.NoError(t, err)
			switch fault {
			case "restore":
				_, err = f.raw.Put(f.ctx, f.b.restoreKey, "restored")
			case "command-aba":
				key, e := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
				require.NoError(t, e)
				_, err = f.raw.Delete(f.ctx, key)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, key, archive.owner.prepared.draft.value)
			case "operation":
				_, err = f.raw.Put(f.ctx, archive.owner.prefix+"new", "new")
			case "local-loss":
				f.c.mu.Lock()
				f.c.lost = true
				f.c.mu.Unlock()
			case "failed-query":
				original := f.b.client.KV
				archive.owner.nativeMu.Lock()
				f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(context.Context, []clientv3.Cmp, []clientv3.Op, []clientv3.Op) (*clientv3.TxnResponse, error) {
					return nil, errors.New("controlled native failure")
				}}
				archive.owner.nativeMu.Unlock()
				defer func() { f.b.client.KV = original }()
			}
			require.NoError(t, err)
			_, err = source.next(f.ctx)
			require.Error(t, err)
			require.NoError(t, source.Close())
		})
	}
}

func TestTaskWorkspaceSyncSourceNativeRenewedPastActualDeadline(t *testing.T) {
	f, archive, plan := syncSourceNativeFixture(t, 3*time.Second)
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.NoError(t, err)
	old := *archive.owner.deadline.Load()
	time.Sleep(600 * time.Millisecond)
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	renewed := *archive.owner.deadline.Load()
	require.True(t, renewed.After(old))
	time.Sleep(time.Until(old) + 40*time.Millisecond)
	_, err = source.next(f.ctx)
	require.NoError(t, err)
	require.True(t, time.Now().After(old))
	t.Logf("real TTL=%ds old=%s renewed=%s observed=%s", f.c.ttlSeconds, old.UTC().Format(time.RFC3339Nano), renewed.UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, source.Close())
}

func TestTaskWorkspaceSyncSourceNativeAfterOriginalSessionExpiry(t *testing.T) {
	f, archive, plan := syncSourceNativeFixture(t, 90*time.Second)
	oldSession := archive.witness.diagnostics.SessionAfter
	oldPreparation := archive.owner.prepared.draft.deadline
	// Actual original signed 45sec session expires; no fixture timestamp rewrite.
	time.Sleep(time.Until(oldSession) + 40*time.Millisecond)
	require.Error(t, archive.owner.ctx.Err(), "historical receive ctx is actually expired")
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.NoError(t, err)
	_, err = source.next(f.ctx)
	require.NoError(t, err)
	entry, err := source.next(f.ctx)
	require.NoError(t, err)
	body, err := source.openBody(f.ctx, entry)
	require.NoError(t, err)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "hello", string(data))
	require.NoError(t, body.Close())
	require.Equal(t, oldSession, archive.witness.diagnostics.SessionAfter)
	require.Equal(t, oldPreparation, archive.owner.prepared.draft.deadline)
	t.Logf("signed_session_after=%s source_observed=%s current_claim_deadline=%s", oldSession.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), archive.owner.deadline.Load().UTC().Format(time.RFC3339Nano))
	require.NoError(t, source.Close())
}

func TestTaskWorkspaceSyncSourceNativePublishedExpiryIndependentOfBlockedRenew(t *testing.T) {
	f, archive, plan := syncSourceNativeFixture(t, 30*time.Second)
	source, err := f.b.PrepareTaskWorkspaceSyncSource(f.ctx, archive, plan)
	require.NoError(t, err)
	// Controlled monotonic local expiry, separately from actual TTL3sec renewal.
	f.c.mu.Lock()
	old := time.Now().Add(180 * time.Millisecond)
	f.c.deadline = old
	archive.owner.deadline.Store(&old)
	f.c.mu.Unlock()
	select {
	case archive.owner.wake <- struct{}{}:
	default:
	}
	original := f.b.client.Lease
	lease := &deliveryBlockedRenewLease{Lease: original, entered: make(chan struct{}), returned: make(chan struct{})}
	f.b.client.Lease = lease
	defer func() { f.b.client.Lease = original }()
	// Renew holds c.mu and its request lasts beyond this published expiry.
	renewed := make(chan error, 1)
	go func() { renewed <- f.b.RenewTaskClaim(f.ctx, f.c) }()
	<-lease.entered
	select {
	case <-source.ctx.Done():
	case <-time.After(350 * time.Millisecond):
		t.Fatal("source expiry waited on claim mutex")
	}
	select {
	case <-lease.returned:
		t.Fatal("expiry needed renewal completion")
	default:
	}
	require.Error(t, <-renewed)
	require.NoError(t, source.Close())
}
