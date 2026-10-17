package mounter

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

// A copied locked mutex must never be reached by Status's native binding
// rejection. This pointer-only owner models instance ownership, not native
// authority: it contains no kernel handles and is not a factory positive case.
func TestNativeAdmissionCopiedStatusRejectsBeforeCopiedMutex(t *testing.T) {
	supervisor, _ := newTestSupervisor(t, &fakeRunner{})
	owner := bindResidentAdmissionOwner(supervisor)
	supervisor.mu.Lock()
	var copied Supervisor
	reflect.ValueOf(&copied).Elem().Set(reflect.ValueOf(supervisor).Elem())
	supervisor.mu.Unlock()

	result := make(chan fuseprotocol.MounterStatus, 1)
	go func() { result <- copied.Status() }()
	var status fuseprotocol.MounterStatus
	select {
	case status = <-result:
	case <-time.After(100 * time.Millisecond):
		// The old Status blocks on this copied lock. Explicitly release it
		// and join that exact call before reporting RED; leave no goroutine
		// parked forever just because the regression assertion failed.
		copied.mu.Unlock()
		status = <-result
		t.Error("copied Status blocked behind a copied mutex")
	}

	assert.Equal(t, string(StateUnhealthy), status.State)
	assert.Empty(t, status.MountType)
	assert.False(t, status.RestartDetected)
	assert.False(t, owner.revoked, "rejecting a copy must not revoke the original owner")
	assert.False(t, owner.closed, "rejecting a copy must not close the original owner")
	assert.Same(t, supervisor, owner.supervisor)
}

// Resident readiness must immediately lose a previously positive sample when
// admission is sealed, and refresh must not republish that old ready identity.
// These are resident-state models; no fake FD or kernel validator is supplied.
func TestNativeAdmissionSealInvalidatesResidentReadiness(t *testing.T) {
	for _, mode := range []string{"revoked", "closed", "busy-close"} {
		t.Run(mode, func(t *testing.T) {
			supervisor, mount := residentReadySupervisor(t)
			observer := sampledResidentObserver(supervisor)
			require.True(t, observer.ready(time.Now()))
			owner := bindResidentAdmissionOwner(supervisor)
			// Binding alone changes no resident state. In particular, this
			// request must not try to validate the model's absent kernel FDs.
			require.True(t, observer.ready(time.Now()))

			if mode == "busy-close" {
				require.ErrorIs(t, supervisor.Close(), errNativeClosePending)
				require.True(t, owner.revoked)
				require.False(t, owner.closed)
			} else {
				supervisor.mu.Lock()
				if mode == "revoked" {
					owner.revoked = true
				} else {
					owner.closed = true
				}
				supervisor.mu.Unlock()
			}

			assert.False(t, observer.ready(time.Now()), "a sealed owner retained a positive sample")
			_, err := observer.refresh()
			require.NoError(t, err)
			assert.Nil(t, observer.sample.Load(), "refresh republished readiness after admission was sealed")
			assert.False(t, observer.ready(time.Now()))
			assert.Equal(t, string(StateUnhealthy), supervisor.Status().State)

			// Preserve the original cleanup path even though management and
			// resident readiness now reject admission. The original child is
			// joined by Shutdown, rather than by a test-only replacement call.
			runner := supervisor.runner.(*fakeRunner)
			supervisor.mu.Lock()
			supervisor.config.MountInfo = func() (Mount, error) {
				return Mount{MountPoint: mount.MountPoint, FilesystemType: "tmpfs"}, nil
			}
			runner.process.signal = func(os.Signal) error {
				runner.process.exit <- nil
				return nil
			}
			supervisor.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err = supervisor.Shutdown(ctx, nil)
			require.NoError(t, err)
			require.Equal(t, StateStopped, supervisor.State())
			require.Same(t, supervisor, owner.supervisor)
		})
	}
}

// bindResidentAdmissionOwner exercises only the private pointer binding and
// seal flags used by resident readers. It cannot validate a native incarnation
// and must never be presented as a native constructor or kernel identity proof.
func bindResidentAdmissionOwner(supervisor *Supervisor) *nativeOwner {
	owner := &nativeOwner{supervisor: supervisor}
	owner.self = owner
	supervisor.nativeOwner = owner
	return owner
}
