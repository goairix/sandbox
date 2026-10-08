package etcd

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskArchiveDeliveryAbsentStore(t *testing.T) {
	b := &Backend{}
	w, err := b.ReceiveTaskWorkspaceArchive(context.Background(), nil)
	require.ErrorIs(t, err, ErrInvalidConfiguration, "delivery must refuse an unconfigured original store")
	require.Nil(t, w)
}

func deliverySpoolOptions(directory string) *TaskWorkspaceArchiveSpoolOptions {
	l := archiveTestLimits()
	return &TaskWorkspaceArchiveSpoolOptions{Directory: directory, Jobs: l.Jobs, JobBytes: l.JobBytes, TotalBytes: l.TotalBytes, Entries: l.Entries, JobIndexBytes: l.JobIndexBytes, TotalIndexBytes: l.TotalIndexBytes}
}

func TestTaskArchiveDeliveryOptionsPureValidation(t *testing.T) {
	directory := archiveTestDirectory(t)
	// A locked nonempty directory remains untouched by pure option validation.
	s, err := newTaskArchiveStore(directory, archiveTestLimits())
	require.NoError(t, err)
	defer s.close()
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	defer r.close()
	o := validOptions(t)
	o.TaskWorkspaceArchiveIssuer = &unusedArchiveIssuer{}
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("spool validation invoked authority clock") })
	o.TaskWorkspaceArchiveSpool = deliverySpoolOptions(directory)
	require.NoError(t, validateOptions(o))
	for _, change := range []func(*TaskWorkspaceArchiveSpoolOptions){
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.Jobs = 0 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.JobBytes = 1535 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.JobBytes = 1<<40 + 1 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.TotalBytes = o.JobBytes - 1 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.Entries = 0 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.Entries = 1000001 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.JobIndexBytes = 0 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.TotalIndexBytes = 0 },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.Directory = "relative" },
		func(o *TaskWorkspaceArchiveSpoolOptions) { o.Directory = directory + "/../other" },
	} {
		copy := *o.TaskWorkspaceArchiveSpool
		change(&copy)
		o.TaskWorkspaceArchiveSpool = &copy
		b, err := New(context.Background(), o)
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		require.Nil(t, b)
		o.TaskWorkspaceArchiveSpool = deliverySpoolOptions(directory)
	}
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, r.name, entries[0].Name())
	require.EqualValues(t, 1, s.jobs)
}

func TestTaskArchiveDeliveryReceiverGuardsZeroCommand(t *testing.T) {
	for _, boundary := range []string{"open", "send"} {
		t.Run(boundary, func(t *testing.T) {
			e, done := archiveConsumerFixture(t, archiveValidTar(t), 4, "")
			s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
			require.NoError(t, err)
			defer s.close()
			r, err := s.reserve(context.Background())
			require.NoError(t, err)
			guard := func(context.Context) error { return ErrConflict }
			if boundary == "open" {
				e.beforeOpen = guard
			} else {
				e.beforeSend = guard
			}
			w, err := r.receive(context.Background(), e)
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, w)
			require.True(t, r.started)
			again, err := r.receive(context.Background(), e)
			require.Error(t, err)
			require.Nil(t, again)
			if boundary == "send" {
				require.Error(t, <-done, "authenticated server received no archive request")
			}
			require.NoError(t, r.close())
		})
	}
}

func TestTaskArchiveDeliveryQuotaUncertainCleanup(t *testing.T) {
	directory := archiveTestDirectory(t)
	s, err := newTaskArchiveStore(directory, archiveTestLimits())
	require.NoError(t, err)
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	// Rename the original inode away: uncertainty must retain its exact charge.
	require.NoError(t, os.Rename(filepath.Join(directory, r.name), filepath.Join(directory, "unsettled")))
	require.Error(t, r.close())
	require.EqualValues(t, 1, s.jobs)
	require.Equal(t, s.limits.JobBytes, s.disk)
	require.Equal(t, s.indexBytes, s.memory)
	require.Error(t, s.close())
	_, err = os.Stat(filepath.Join(directory, "unsettled"))
	require.NoError(t, err)
}

func TestTaskArchiveDeliveryResultAndBackendPhysicalClose(t *testing.T) {
	control := &archiveCloseControl{entered: make(chan struct{}), release: make(chan struct{})}
	e, _ := archiveConsumerFixture(t, archiveValidTar(t), 4, "", control)
	s, err := newTaskArchiveStore(archiveTestDirectory(t), archiveTestLimits())
	require.NoError(t, err)
	b := &Backend{archiveStore: s, archiveLife: &taskArchiveBackend{}}
	r, err := s.reserve(context.Background())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	o := &taskArchiveDelivery{backend: b, spool: r, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	b.archiveLife.owners = map[*taskArchiveDelivery]struct{}{o: {}}
	received := make(chan error, 1)
	go func() {
		_, err := r.receive(ctx, e)
		received <- err
		close(o.done)
	}()
	<-control.entered
	closed := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); closed <- b.Close() }()
	}
	select {
	case <-closed:
		t.Fatal("Backend.Close returned before physical Session.Close")
	case <-time.After(40 * time.Millisecond):
	}
	close(control.release)
	wg.Wait()
	require.NoError(t, <-closed)
	require.NoError(t, <-closed)
	require.Error(t, <-received)
	require.Zero(t, s.jobs)
	var zero *ReceivedTaskWorkspaceArchive
	require.Error(t, zero.Close())
}
