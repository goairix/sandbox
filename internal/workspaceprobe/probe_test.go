package workspaceprobe

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProcesses struct {
	snapshots [][]processIdentity
	scans     int
	stopped   []int
	resumed   []int
	fds       map[int][]descriptor
	states    map[int]processIdentity
}

func (f *fakeProcesses) listSameUID(_ int, _ int) ([]processIdentity, error) {
	if len(f.snapshots) == 0 {
		return nil, nil
	}
	i := f.scans
	if i >= len(f.snapshots) {
		i = len(f.snapshots) - 1
	}
	f.scans++
	return append([]processIdentity(nil), f.snapshots[i]...), nil
}

func (f *fakeProcesses) signal(pid int, signal syscall.Signal) error {
	switch signal {
	case syscall.SIGSTOP:
		f.stopped = append(f.stopped, pid)
		state := f.states[pid]
		state.State = 'T'
		f.states[pid] = state
	case syscall.SIGCONT:
		f.resumed = append(f.resumed, pid)
	}
	return nil
}

func (f *fakeProcesses) identity(pid int) (processIdentity, error) {
	identity, ok := f.states[pid]
	if !ok {
		return processIdentity{}, os.ErrNotExist
	}
	return identity, nil
}

func (f *fakeProcesses) descriptors(pid int) ([]descriptor, error) {
	return append([]descriptor(nil), f.fds[pid]...), nil
}

type fakeBroker struct {
	state     brokerState
	token     string
	resumes   []resumeRequest
	consumed  bool
	protected *processIdentity
}

func (f *fakeBroker) protectedProcess() (*processIdentity, error) { return f.protected, nil }

type fakeMount struct {
	err   error
	calls int
}

type workspaceIOBoundaryFunc func(string, int64) error

func (f workspaceIOBoundaryFunc) run(runtimeUID string, generation int64) error {
	return f(runtimeUID, generation)
}

func (f *fakeMount) requireS3FSMount(string) error {
	f.calls++
	return f.err
}

func (f *fakeBroker) start(state brokerState) (string, error) {
	f.state = state
	return f.token, nil
}

func (f *fakeBroker) resume(request resumeRequest) error {
	f.resumes = append(f.resumes, request)
	if f.consumed || request.Token != f.token || request.RuntimeUID != f.state.RuntimeUID || request.Generation != f.state.Generation {
		return ErrInvalidToken
	}
	f.consumed = true
	return nil
}

func testProbe(t *testing.T) (*Probe, *fakeProcesses, *fakeBroker) {
	t.Helper()
	processes := &fakeProcesses{
		states: map[int]processIdentity{},
		fds:    map[int][]descriptor{},
	}
	broker := &fakeBroker{token: "opaque-token"}
	mount := &fakeMount{}
	probe := newProbe(config{
		workspace: filepath.Join(t.TempDir(), "workspace"),
		uid:       1000,
		gid:       1000,
		selfPID:   99,
		processes: processes,
		broker:    broker,
		mounts:    mount,
		random:    bytes.NewReader(bytes.Repeat([]byte{0x42}, 256)),
		euid:      func() int { return 1000 },
		egid:      func() int { return 1000 },
		chdir:     func(string) error { return nil },
	})
	probe.ioBoundary = workspaceIOBoundaryFunc(func(runtimeUID string, generation int64) error {
		name, err := fuseprotocol.DeriveProbeObjectName(runtimeUID, generation)
		if err != nil {
			return err
		}
		return probe.writeReadDeleteDirect(runtimeUID, generation, name)
	})
	require.NoError(t, os.Mkdir(probe.workspace, 0o755))
	return probe, processes, broker
}

func TestWriteReadDeleteRequiresEffectiveS3FSMount(t *testing.T) {
	probe, _, _ := testProbe(t)
	mount := &fakeMount{err: errors.New("not fuse.s3fs")}
	probe.mounts = mount
	_, err := probe.WriteReadDelete("runtime-a", 7)
	require.ErrorContains(t, err, "not fuse.s3fs")
	entries, readErr := os.ReadDir(probe.workspace)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestWriteReadDeletePropagatesHardBoundaryFailure(t *testing.T) {
	probe, _, _ := testProbe(t)
	probe.ioBoundary = workspaceIOBoundaryFunc(func(string, int64) error { return ErrIOTimeout })
	status, err := probe.WriteReadDelete("runtime-a", 7)
	require.ErrorIs(t, err, ErrIOTimeout)
	assert.False(t, status.OK)
}

func TestCleanupRemovesOnlyExactParentGeneratedProbeName(t *testing.T) {
	probe, _, _ := testProbe(t)
	target, err := fuseprotocol.DeriveProbeObjectName("runtime-a", 7)
	require.NoError(t, err)
	unrelated, err := fuseprotocol.DeriveProbeObjectName("runtime-b", 7)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(probe.workspace, target), []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(probe.workspace, unrelated), []byte("user"), 0o600))

	require.NoError(t, probe.cleanupProbeDirect("runtime-a", 7, target))
	_, err = os.Stat(filepath.Join(probe.workspace, target))
	require.ErrorIs(t, err, os.ErrNotExist)
	contents, err := os.ReadFile(filepath.Join(probe.workspace, unrelated))
	require.NoError(t, err)
	assert.Equal(t, "user", string(contents))
}

func TestWriteReadDeleteUsesFixedWorkspaceAndLeavesNoFile(t *testing.T) {
	probe, _, _ := testProbe(t)
	status, err := probe.WriteReadDelete("runtime-a", 7)
	require.NoError(t, err)
	assert.True(t, status.OK)
	assert.Equal(t, "runtime-a", status.RuntimeUID)
	assert.Equal(t, int64(7), status.Generation)
	entries, err := os.ReadDir(probe.workspace)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestWriteReadDeleteRefusesExistingProbeFile(t *testing.T) {
	probe, _, _ := testProbe(t)
	name, err := fuseprotocol.DeriveProbeObjectName("runtime-a", 7)
	require.NoError(t, err)
	path := filepath.Join(probe.workspace, name)
	require.NoError(t, os.WriteFile(path, []byte("attacker"), 0o600))
	_, err = probe.WriteReadDelete("runtime-a", 7)
	require.Error(t, err)
	contents, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "attacker", string(contents))
}

func TestQuiesceStopsToFixedPointAndStartsBoundBroker(t *testing.T) {
	probe, processes, broker := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'S'}
	p11 := processIdentity{PID: 11, UID: 1000, StartTime: 102, State: 'S'}
	processes.snapshots = [][]processIdentity{{p10}, {{PID: 10, UID: 1000, StartTime: 101, State: 'T'}, p11}, {
		{PID: 10, UID: 1000, StartTime: 101, State: 'T'},
		{PID: 11, UID: 1000, StartTime: 102, State: 'T'},
	}}
	processes.states[10] = p10
	processes.states[11] = p11

	status, err := probe.Quiesce("runtime-a", 7)
	require.NoError(t, err)
	assert.Equal(t, []int{10, 11}, processes.stopped)
	assert.Equal(t, "opaque-token", status.Token)
	assert.Equal(t, "runtime-a", broker.state.RuntimeUID)
	assert.Equal(t, int64(7), broker.state.Generation)
	assert.ElementsMatch(t, []processIdentity{
		{PID: 10, UID: 1000, StartTime: 101, State: 'T'},
		{PID: 11, UID: 1000, StartTime: 102, State: 'T'},
	}, broker.state.Processes)
}

func TestQuiesceExcludesOnlyVerifiedPID1Ledger(t *testing.T) {
	probe, processes, broker := testProbe(t)
	pid1 := processIdentity{PID: 1, UID: 1000, StartTime: 10, State: 'S'}
	user := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'S'}
	broker.protected = &pid1
	processes.snapshots = [][]processIdentity{{pid1, user}, {pid1, {PID: 10, UID: 1000, StartTime: 101, State: 'T'}}}
	processes.states[1] = pid1
	processes.states[10] = user

	_, err := probe.Quiesce("runtime-a", 7)
	require.NoError(t, err)
	assert.Equal(t, []int{10}, processes.stopped)
	assert.Equal(t, []processIdentity{{PID: 10, UID: 1000, StartTime: 101, State: 'T'}}, broker.state.Processes)
}

func TestQuiesceWritableWorkspaceFDFailsClosedWithProcessesStopped(t *testing.T) {
	probe, processes, broker := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'S'}
	processes.snapshots = [][]processIdentity{{p10}, {{PID: 10, UID: 1000, StartTime: 101, State: 'T'}}}
	processes.states[10] = p10
	processes.fds[10] = []descriptor{{Target: filepath.Join(probe.workspace, "open"), Flags: syscall.O_WRONLY}}

	_, err := probe.Quiesce("runtime-a", 7)
	require.ErrorIs(t, err, ErrWritableWorkspaceFD)
	assert.Empty(t, processes.resumed)
	assert.Equal(t, []int{10}, processes.stopped)
	assert.Empty(t, broker.state.Processes)
}

func TestQuiesceAllowsReadOnlyWorkspaceFDAndWritableOutside(t *testing.T) {
	probe, processes, _ := testProbe(t)
	p10 := processIdentity{PID: 10, UID: 1000, StartTime: 101, State: 'S'}
	processes.snapshots = [][]processIdentity{{p10}, {{PID: 10, UID: 1000, StartTime: 101, State: 'T'}}}
	processes.states[10] = p10
	processes.fds[10] = []descriptor{
		{Target: filepath.Join(probe.workspace, "read"), Flags: syscall.O_RDONLY},
		{Target: "/tmp/write", Flags: syscall.O_WRONLY},
	}

	_, err := probe.Quiesce("runtime-a", 7)
	require.NoError(t, err)
}

func TestResumeRejectsCrossRuntimeGenerationAndReplay(t *testing.T) {
	probe, _, broker := testProbe(t)
	broker.state = brokerState{RuntimeUID: "runtime-a", Generation: 7}

	for _, request := range []resumeRequest{
		{RuntimeUID: "runtime-b", Generation: 7, Token: broker.token},
		{RuntimeUID: "runtime-a", Generation: 8, Token: broker.token},
		{RuntimeUID: "runtime-a", Generation: 7, Token: "wrong"},
	} {
		err := probe.Resume(request)
		require.ErrorIs(t, err, ErrInvalidToken)
	}
	require.NoError(t, probe.Resume(resumeRequest{RuntimeUID: "runtime-a", Generation: 7, Token: broker.token}))
	require.ErrorIs(t, probe.Resume(resumeRequest{RuntimeUID: "runtime-a", Generation: 7, Token: broker.token}), ErrInvalidToken)
}

func TestValidateIdentityAndExecutionUser(t *testing.T) {
	probe, _, _ := testProbe(t)
	probe.euid = func() int { return 0 }
	_, err := probe.WriteReadDelete("runtime-a", 7)
	require.ErrorIs(t, err, ErrWrongUser)

	probe.euid = func() int { return 1000 }
	probe.egid = func() int { return 1000 }
	_, err = probe.WriteReadDelete("", 7)
	require.Error(t, err)
	_, err = probe.WriteReadDelete("runtime-a", 0)
	require.Error(t, err)
}

func TestQuiesceEnumerationFailureDoesNotStartBroker(t *testing.T) {
	probe, processes, broker := testProbe(t)
	processes.snapshots = nil
	probe.processes = failingProcesses{err: errors.New("proc unavailable")}
	_, err := probe.Quiesce("runtime-a", 7)
	require.ErrorContains(t, err, "proc unavailable")
	assert.Empty(t, broker.state.Processes)
}

type failingProcesses struct{ err error }

func (f failingProcesses) listSameUID(int, int) ([]processIdentity, error) { return nil, f.err }
func (f failingProcesses) signal(int, syscall.Signal) error                { return f.err }
func (f failingProcesses) identity(int) (processIdentity, error)           { return processIdentity{}, f.err }
func (f failingProcesses) descriptors(int) ([]descriptor, error)           { return nil, f.err }
