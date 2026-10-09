package workspaceprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcEnumerationExcludesExitedProcessesButKeepsBlockedProcesses(t *testing.T) {
	root := t.TempDir()
	for pid, state := range map[int]byte{10: 'Z', 11: 'X', 12: 'x', 13: 'D', 14: 'S', 15: 'T'} {
		writeProcGroup(t, root, pid, []procTaskFixture{{tid: pid, state: state, uid: 1000, start: 98765}})
	}
	processes, err := (procFS{root: root}).listSameUID(1000, 99)
	require.NoError(t, err)
	var pids []int
	for _, process := range processes {
		pids = append(pids, process.PID)
	}
	assert.ElementsMatch(t, []int{13, 14, 15}, pids, "exited processes cannot execute; a blocked live process must still prevent unsafe quiescence")
}

func TestProcEnumerationRetainsZombieLeaderWithLiveSiblingThreads(t *testing.T) {
	root := t.TempDir()
	writeProcGroup(t, root, 10, []procTaskFixture{
		{tid: 10, state: 'Z', uid: 1000, start: 98765},
		{tid: 11, state: 'S', uid: 1000, start: 98766},
	})
	processes, err := (procFS{root: root}).listSameUID(1000, 99)
	require.NoError(t, err)
	require.Len(t, processes, 1, "leader exit must not hide a live worker from quiescence")
	assert.Equal(t, 10, processes[0].PID)
}

func TestParseProcStatHandlesParenthesesAndSpaces(t *testing.T) {
	identity, err := parseProcStat(42, []byte("42 (odd ) process) T 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 98765 20\n"))
	require.NoError(t, err)
	assert.Equal(t, 42, identity.PID)
	assert.Equal(t, byte('T'), identity.State)
	assert.Equal(t, uint64(98765), identity.StartTime)
}

func TestParseProcStatRejectsUnknownThreadCount(t *testing.T) {
	for _, count := range []string{"0", "-1", "unknown"} {
		_, err := parseProcStat(42, []byte(fmt.Sprintf("42 (worker) Z 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 %s 18 98765 20\n", count)))
		require.Error(t, err, "unknown thread count must not prove the entire process exited")
	}
}

func TestProcDescriptorsReadFlagsAndTargets(t *testing.T) {
	root := t.TempDir()
	writeProcGroup(t, root, 10, []procTaskFixture{{tid: 10, state: 'T', uid: 1000, start: 98765}})
	fdDir := filepath.Join(root, "10", "task", "10", "fd")
	fdInfoDir := filepath.Join(root, "10", "task", "10", "fdinfo")
	require.NoError(t, os.MkdirAll(fdDir, 0o755))
	require.NoError(t, os.MkdirAll(fdInfoDir, 0o755))
	require.NoError(t, os.Symlink("/workspace/data", filepath.Join(fdDir, "3")))
	require.NoError(t, os.WriteFile(filepath.Join(fdInfoDir, "3"), []byte("pos:\t0\nflags:\t0100001\n"), 0o600))

	proc := procFS{root: root}
	fds, err := proc.descriptors(10)
	require.NoError(t, err)
	require.Len(t, fds, 1)
	assert.Equal(t, "/workspace/data", fds[0].Target)
	assert.Equal(t, syscall.O_WRONLY, fds[0].Flags&syscall.O_ACCMODE)
}

func TestWorkspaceTargetRequiresPathBoundary(t *testing.T) {
	assert.True(t, targetInWorkspace("/workspace/file", "/workspace"))
	assert.True(t, targetInWorkspace("/workspace/file (deleted)", "/workspace"))
	assert.False(t, targetInWorkspace("/workspace-escape/file", "/workspace"))
}

func TestParseProcessUIDUsesEffectiveIdentity(t *testing.T) {
	uid, err := parseEffectiveUID([]byte("Name:\ttest\nUid:\t501\t1000\t1000\t1000\n"))
	require.NoError(t, err)
	assert.Equal(t, 1000, uid)
}

// Linux enumerates thread groups at /proc/PID and individual task state and
// private descriptor tables below /proc/PID/task/TID.
type procTaskFixture struct {
	tid   int
	state byte
	uid   int
	start uint64
}

func writeProcTask(t *testing.T, dir string, task procTaskFixture, threads int) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (worker) %c 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 %d 18 %d 20\n", task.tid, task.state, threads, task.start)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "status"), []byte(fmt.Sprintf("Uid:\t%d\t%d\t%d\t%d\n", task.uid, task.uid, task.uid, task.uid)), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fd"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "fdinfo"), 0o755))
}

func writeProcGroup(t *testing.T, root string, pid int, tasks []procTaskFixture) {
	t.Helper()
	leader := filepath.Join(root, fmt.Sprint(pid))
	for _, task := range tasks {
		writeProcTask(t, filepath.Join(leader, "task", fmt.Sprint(task.tid)), task, len(tasks))
		if task.tid == pid {
			// These paths are aliases in the kernel, including the leader FD table.
			for _, name := range []string{"stat", "status", "fd", "fdinfo"} {
				require.NoError(t, os.Symlink(filepath.Join("task", fmt.Sprint(pid), name), filepath.Join(leader, name)))
			}
		}
	}
}

func TestReviewWritablePrivateThreadFDMustBeRejected(t *testing.T) {
	root := t.TempDir()
	writeProcGroup(t, root, 10, []procTaskFixture{
		{tid: 10, state: 'T', uid: 1000, start: 98765},
		{tid: 11, state: 'T', uid: 1000, start: 98766},
	})
	worker := filepath.Join(root, "10", "task", "11")
	require.NoError(t, os.Symlink("/workspace/data", filepath.Join(worker, "fd", "3")))
	require.NoError(t, os.WriteFile(filepath.Join(worker, "fdinfo", "3"), []byte("pos:\t0\nflags:\t0100001\n"), 0o600))
	probe, _, _ := testProbe(t)
	probe.workspace = "/workspace"
	probe.processes = fixtureProcSignals{procFS{root: root}}
	status, err := probe.QuiesceForCleanup("runtime-a", 7)
	require.ErrorIs(t, err, ErrWritableWorkspaceFD, "CLONE_THREAD without CLONE_FILES gives each task a distinct FD table")
	assert.False(t, status.OK)
}

type fixtureProcSignals struct{ procFS }

func (p fixtureProcSignals) signal(int, syscall.Signal) error { return nil }

func TestProcIdentityRequiresEveryLiveTaskToStop(t *testing.T) {
	for _, test := range []struct {
		name           string
		leader, worker byte
		wantStopped    bool
	}{
		{"blocked worker", 'T', 'D', false},
		{"running worker", 'T', 'R', false},
		{"stopped siblings of dead leader", 'Z', 'T', true},
		{"traced sibling of dead leader", 'Z', 't', true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeProcGroup(t, root, 10, []procTaskFixture{
				{tid: 10, state: test.leader, uid: 1000, start: 98765},
				{tid: 11, state: test.worker, uid: 1000, start: 98766},
			})
			identity, err := (procFS{root: root}).identity(10)
			require.NoError(t, err)
			assert.Equal(t, test.wantStopped, isStopped(identity.State))
			assert.Equal(t, 10, identity.PID)
			assert.Equal(t, uint64(98765), identity.StartTime, "broker identity remains the group leader")
			assert.Equal(t, 1000, identity.UID)
		})
	}
}

func TestProcMissingOrIncompleteTaskSetFailsClosed(t *testing.T) {
	for _, mutation := range []string{"missing task directory", "missing task stat", "incorrect thread count"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			writeProcGroup(t, root, 10, []procTaskFixture{{tid: 10, state: 'T', uid: 1000, start: 98765}})
			dir := filepath.Join(root, "10", "task", "10")
			switch mutation {
			case "missing task directory":
				// Keep leader data as /proc/PID normally presents it.
				require.NoError(t, os.Rename(filepath.Join(root, "10", "task"), filepath.Join(root, "10", "hidden")))
				for _, name := range []string{"stat", "status", "fd", "fdinfo"} {
					require.NoError(t, os.Remove(filepath.Join(root, "10", name)))
					require.NoError(t, os.Symlink(filepath.Join("hidden", "10", name), filepath.Join(root, "10", name)))
				}
			case "missing task stat":
				require.NoError(t, os.Remove(filepath.Join(dir, "status")))
			case "incorrect thread count":
				writeProcTask(t, dir, procTaskFixture{tid: 10, state: 'T', uid: 1000, start: 98765}, 2)
			}
			_, err := (procFS{root: root}).listSameUID(1000, 99)
			require.Error(t, err, "incomplete group evidence must not silently exclude a live process")
		})
	}
}

func TestProcDescriptorScanRejectsTaskMutation(t *testing.T) {
	for _, mutation := range []string{"exited", "reused", "uid changed", "resumed", "task added"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			writeProcGroup(t, root, 10, []procTaskFixture{
				{tid: 10, state: 'T', uid: 1000, start: 98765},
				{tid: 11, state: 'T', uid: 1000, start: 98766},
			})
			// Pause the actual descriptor read to mutate the task set between
			// the initial evidence and the final validation, without hooks.
			leader := filepath.Join(root, "10", "task", "10")
			require.NoError(t, os.Symlink("/tmp/read-only", filepath.Join(leader, "fd", "3")))
			fifo := filepath.Join(leader, "fdinfo", "3")
			require.NoError(t, syscall.Mkfifo(fifo, 0o600))
			done := make(chan error, 1)
			go func() {
				_, err := (procFS{root: root}).descriptors(10)
				done <- err
			}()
			writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
			require.NoError(t, err)
			worker := filepath.Join(root, "10", "task", "11")
			switch mutation {
			case "exited":
				require.NoError(t, os.RemoveAll(worker))
			case "reused":
				writeProcTask(t, worker, procTaskFixture{tid: 11, state: 'T', uid: 1000, start: 99999}, 2)
			case "uid changed":
				writeProcTask(t, worker, procTaskFixture{tid: 11, state: 'T', uid: 1001, start: 98766}, 2)
			case "resumed":
				writeProcTask(t, worker, procTaskFixture{tid: 11, state: 'R', uid: 1000, start: 98766}, 2)
			case "task added":
				writeProcTask(t, filepath.Join(root, "10", "task", "12"), procTaskFixture{tid: 12, state: 'T', uid: 1000, start: 98767}, 3)
			}
			_, err = writer.WriteString("flags:\t0100000\n")
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			select {
			case err := <-done:
				require.Error(t, err, "changed task evidence cannot authorize a descriptor proof")
			case <-time.After(5 * time.Second):
				t.Fatal("descriptor scan did not finish")
			}
		})
	}
}
