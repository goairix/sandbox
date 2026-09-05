package workspaceprobe

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIOChild struct {
	mu            sync.Mutex
	exited        bool
	exitErr       error
	killMakesExit bool
	kills         int
	releases      int
	pollErrors    int
}

func (f *fakeIOChild) poll() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pollErrors > 0 {
		f.pollErrors--
		return false, errors.New("wait status unavailable")
	}
	return f.exited, f.exitErr
}

func TestBoundedIOPollFailureKillsAndReapsHelper(t *testing.T) {
	child := &fakeIOChild{pollErrors: 1, killMakesExit: true}
	err := waitBoundedIO(child, time.Second, 50*time.Millisecond, time.Millisecond)
	require.ErrorContains(t, err, "wait status unavailable")
	assert.Equal(t, 1, child.kills)
	assert.Equal(t, 1, child.releases)
}

func (f *fakeIOChild) kill() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kills++
	if f.killMakesExit {
		f.exited = true
		f.exitErr = errors.New("killed")
	}
	return nil
}

func (f *fakeIOChild) release() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	return nil
}

func TestBoundedIOTimeoutKillsAndReapsHelper(t *testing.T) {
	child := &fakeIOChild{killMakesExit: true}
	started := time.Now()
	err := waitBoundedIO(child, 10*time.Millisecond, 50*time.Millisecond, time.Millisecond)
	require.ErrorIs(t, err, ErrIOTimeout)
	assert.Less(t, time.Since(started), 200*time.Millisecond)
	assert.Equal(t, 1, child.kills)
	assert.Equal(t, 1, child.releases)
}

func TestBoundedIOUninterruptibleHelperKeepsParentWaitResponsibility(t *testing.T) {
	child := &fakeIOChild{}
	result := make(chan error, 1)
	go func() {
		result <- waitBoundedIO(child, 5*time.Millisecond, 5*time.Millisecond, time.Millisecond)
	}()
	select {
	case err := <-result:
		t.Fatalf("unconfirmed helper returned and could be orphaned: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	child.mu.Lock()
	assert.Equal(t, 1, child.kills)
	assert.Zero(t, child.releases)
	child.exited = true
	child.exitErr = errors.New("killed")
	child.mu.Unlock()
	require.ErrorIs(t, <-result, ErrIOTimeout)
	child.mu.Lock()
	assert.Equal(t, 1, child.releases)
	child.mu.Unlock()
}

func TestBoundedIOSuccessReapsHelper(t *testing.T) {
	child := &fakeIOChild{exited: true}
	require.NoError(t, waitBoundedIO(child, time.Second, time.Second, time.Millisecond))
	assert.Zero(t, child.kills)
	assert.Equal(t, 1, child.releases)
}

func TestInternalIOHelperRequiresExactEnvelopeAndArgv(t *testing.T) {
	probeName, err := fuseprotocol.DeriveProbeObjectName("runtime-a", 7)
	require.NoError(t, err)
	launch := ioHelperLaunch{Version: 1, Operation: "probe", RuntimeUID: "runtime-a", Generation: 7, ProbeName: probeName}
	assert.True(t, validIOHelperInvocation(1000, 1000, ioHelperEnvironmentValue,
		[]string{"/usr/local/bin/workspace-probe", "write-read-delete", "--runtime-uid", "runtime-a", "--generation", "7"}, launch))
	assert.False(t, validIOHelperInvocation(1000, 1000, ioHelperEnvironmentValue,
		[]string{"/usr/local/bin/workspace-probe", "write-read-delete", "--runtime-uid", "runtime-b", "--generation", "7"}, launch))
	assert.False(t, validIOHelperInvocation(1000, 1000, ioHelperEnvironmentValue,
		[]string{"/usr/local/bin/workspace-probe", "quiesce", "--runtime-uid", "runtime-a", "--generation", "7"}, launch))
	launch.ProbeName = "../user-file"
	assert.False(t, validIOHelperInvocation(1000, 1000, ioHelperEnvironmentValue,
		[]string{"/usr/local/bin/workspace-probe", "write-read-delete", "--runtime-uid", "runtime-a", "--generation", "7"}, launch))
	otherRuntimeName, err := fuseprotocol.DeriveProbeObjectName("runtime-b", 7)
	require.NoError(t, err)
	launch.ProbeName = otherRuntimeName
	assert.False(t, validIOHelperInvocation(1000, 1000, ioHelperEnvironmentValue,
		[]string{"/usr/local/bin/workspace-probe", "write-read-delete", "--runtime-uid", "runtime-a", "--generation", "7"}, launch))
}

func TestIOBoundaryCompensatesExactGeneratedPathAfterTimeout(t *testing.T) {
	primary := &fakeIOChild{killMakesExit: true}
	cleanup := &fakeIOChild{exited: true}
	var operations []string
	var names []string
	boundary := &subprocessIOBoundary{
		timeout: 5 * time.Millisecond, killGrace: 50 * time.Millisecond, pollInterval: time.Millisecond,
		start: func(operation, _ string, _ int64, probeName string) (ioChild, error) {
			operations = append(operations, operation)
			names = append(names, probeName)
			if operation == "probe" {
				return primary, nil
			}
			return cleanup, nil
		},
		hold: func() {},
	}
	err := boundary.run("runtime-a", 7)
	require.ErrorIs(t, err, ErrIOTimeout)
	assert.Equal(t, []string{"probe", "cleanup"}, operations)
	require.Len(t, names, 2)
	assert.Equal(t, names[0], names[1])
	expected, err := fuseprotocol.DeriveProbeObjectName("runtime-a", 7)
	require.NoError(t, err)
	assert.Equal(t, expected, names[0])
}

func TestIOBoundaryCleanupFailureEntersFailClosedHold(t *testing.T) {
	primary := &fakeIOChild{exited: true, exitErr: errors.New("write failed")}
	cleanup := &fakeIOChild{exited: true, exitErr: errors.New("delete failed")}
	held := 0
	boundary := &subprocessIOBoundary{
		timeout: time.Second, killGrace: time.Second, pollInterval: time.Millisecond,
		start: func(operation, _ string, _ int64, _ string) (ioChild, error) {
			if operation == "probe" {
				return primary, nil
			}
			return cleanup, nil
		},
		hold: func() { held++ },
	}
	err := boundary.run("runtime-a", 7)
	require.ErrorIs(t, err, ErrIOCompensationUnconfirmed)
	assert.Equal(t, 1, held)
}
