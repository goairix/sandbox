package etcd

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These are canonical metadata values, never a Kernel, recovery source or
// permission to finalize. Actual filesystem validation/Close and new-Run/Claim
// admission remain covered by the genuine Linux bound-recovery case.
type cleanupBoundMatchFixture struct {
	state    *cleanupSlotState
	target   *cleanupLocalTarget
	root     taskArchiveRecoveryRoot
	current  taskWorkspaceSyncLocalAttribution
	manifest taskArchiveRecoveryManifest
}

func cleanupBoundMetadata(t *testing.T) cleanupBoundMatchFixture {
	t.Helper()
	_, input := cleanupLocalWireFixture(t)
	r := input.state.record
	run := *r.Run
	run.Joined = true
	r.Run, r.Pending, r.Confirmed = &run, nil, "archive-bound"
	local := taskWorkspaceSyncLocalAttribution{Worker: run.Worker, Root: "/tmp/cleanup-bound-target", Dev: 1, Ino: 2, MountID: 3, Filesystem: 0x01021994, FilesystemName: "tmpfs", UID: run.Worker.UID, GID: run.Worker.GID}
	localWire, err := encodeOperationMetadata(local, 16384)
	require.NoError(t, err)
	m := recoveryTestManifest(t, "bound")
	m.Seed.Task, m.Seed.TaskBirth, m.Seed.TaskDigest = r.Task, r.TaskBirth, r.TaskDigest
	m.Root = taskArchiveRecoveryRoot{Dev: 4, Ino: 5, MountID: 3, Filesystem: 0x01021994, UID: run.Worker.UID, GID: run.Worker.GID, BootID: run.Worker.BootID, MountNamespace: run.Worker.MountNamespaceIno}
	m.Binding = &taskArchiveRecoveryBinding{Plan: recoveryPlanWire(input.plan), Local: local, TargetDigest: workspaceSyncIntentDigest(localWire)}
	require.NoError(t, m.validate())
	r.Archive = &cleanupExecutionArchive{StoreID: m.StoreID, Slot: m.Slot, BindingDigest: m.Binding.TargetDigest}
	require.NoError(t, r.Validate())
	state := *input.state
	state.record = r
	return cleanupBoundMatchFixture{state: &state, target: &cleanupLocalTarget{plan: input.plan, root: local.Root}, root: m.Root, current: local, manifest: m}
}

func TestCleanupLocalBoundManifest(t *testing.T) {
	f := cleanupBoundMetadata(t)
	require.NoError(t, matchCleanupBoundManifest(f.state, f.target, f.root, f.current, f.manifest))
	t.Run("local-complete-metadata", func(t *testing.T) {
		f := cleanupBoundMetadata(t)
		f.state.record.Confirmed = "local-complete"
		f.state.record.Pending = &cleanupExecutionPending{Purpose: "termination", ClaimID: uuid.NewString(), LeaseID: 7}
		require.NoError(t, f.state.record.Validate())
		require.NoError(t, matchCleanupBoundManifest(f.state, f.target, f.root, f.current, f.manifest))
		// Matching a manifest is only attribution. The phase gate additionally
		// requires the separately authenticated original observer reservation.
		require.False(t, cleanupRecoveryPhase("local-complete", nil))
	})

	// A real replacement worker has a different PID/incarnation. Only the
	// original same-boot/namespaces/target rule can admit that copied attribution;
	// this positive is pure matching, not a host physical worker capability.
	f.current.Worker.Incarnation = uuid.NewString()
	f.current.Worker.PID++
	f.current.Worker.StartTicks++
	require.NoError(t, f.current.Validate())
	require.NoError(t, matchCleanupBoundManifest(f.state, f.target, f.root, f.current, f.manifest))

	for name, mutate := range map[string]func(*cleanupBoundMatchFixture){
		"store-id":               func(f *cleanupBoundMatchFixture) { f.manifest.StoreID = strings.Repeat("e", 64) },
		"archive-slot":           func(f *cleanupBoundMatchFixture) { f.manifest.Slot = (f.manifest.Slot + 1) % 64 },
		"binding-digest":         func(f *cleanupBoundMatchFixture) { f.state.record.Archive.BindingDigest = strings.Repeat("e", 64) },
		"task":                   func(f *cleanupBoundMatchFixture) { f.manifest.Seed.Task.TaskID = uuid.NewString() },
		"task-birth":             func(f *cleanupBoundMatchFixture) { f.manifest.Seed.TaskBirth++ },
		"task-digest":            func(f *cleanupBoundMatchFixture) { f.manifest.Seed.TaskDigest = strings.Repeat("e", 64) },
		"spool-root":             func(f *cleanupBoundMatchFixture) { f.root.Ino++ },
		"target-root":            func(f *cleanupBoundMatchFixture) { f.target.root += "/another" },
		"target-inode":           func(f *cleanupBoundMatchFixture) { f.current.Ino++ },
		"target-mount":           func(f *cleanupBoundMatchFixture) { f.current.MountID++ },
		"worker-boot":            func(f *cleanupBoundMatchFixture) { f.current.Worker.BootID = uuid.NewString() },
		"worker-pid-namespace":   func(f *cleanupBoundMatchFixture) { f.current.Worker.PIDNamespaceIno++ },
		"worker-mount-namespace": func(f *cleanupBoundMatchFixture) { f.current.Worker.MountNamespaceIno++ },
		"worker-uid": func(f *cleanupBoundMatchFixture) {
			f.current.Worker.UID++
			f.current.UID++
		},
		"unjoined":       func(f *cleanupBoundMatchFixture) { f.state.record.Run.Joined = false },
		"unconfirmed":    func(f *cleanupBoundMatchFixture) { f.state.record.Confirmed = "claimed" },
		"terminated":     func(f *cleanupBoundMatchFixture) { f.state.record.Confirmed = "terminated" },
		"owner-released": func(f *cleanupBoundMatchFixture) { f.state.record.Confirmed = "owner-released" },
		"no-binding":     func(f *cleanupBoundMatchFixture) { f.manifest.Binding = nil },
		"no-archive":     func(f *cleanupBoundMatchFixture) { f.state.record.Archive = nil },
		"no-state":       func(f *cleanupBoundMatchFixture) { f.state = nil },
		"no-target":      func(f *cleanupBoundMatchFixture) { f.target = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := cleanupBoundMetadata(t)
			mutate(&f)
			require.Error(t, matchCleanupBoundManifest(f.state, f.target, f.root, f.current, f.manifest))
		})
	}

	for _, policy := range []string{"delete", "exclude", "workspace"} {
		t.Run("plan-"+policy, func(t *testing.T) {
			f := cleanupBoundMetadata(t)
			options := TaskWorkspaceSyncPlanOptions{Workspace: f.target.plan.workspace, Exclude: f.target.plan.exclude, DeleteExtraneous: f.target.plan.deleteExtraneous}
			switch policy {
			case "delete":
				options.DeleteExtraneous = !options.DeleteExtraneous
			case "exclude":
				options.Exclude = []string{"another-policy"}
			case "workspace":
				workspace, err := NewWorkspaceIdentity("local", "different-storage", "bucket", "another/")
				require.NoError(t, err)
				options.Workspace = workspace
			}
			plan, err := NewTaskWorkspaceSyncPlan(options)
			require.NoError(t, err)
			f.target.plan = plan
			require.Error(t, matchCleanupBoundManifest(f.state, f.target, f.root, f.current, f.manifest))
		})
	}
}

func TestCleanupLocalRecoveryPhase(t *testing.T) {
	// A non-nil fence here only selects a mode; the actual history/point check
	// must authenticate it before any new Run or physical recovery is admitted.
	reservation := &taskFence{}
	for _, phase := range []string{"reserved", "claimed", "closed", "quiescent", "drained", "archive-bound", "local-complete", "terminated", "owner-released", "settled", ""} {
		t.Run(phase+"-without-reservation", func(t *testing.T) {
			require.Equal(t, phase == "archive-bound", cleanupRecoveryPhase(phase, nil))
		})
		t.Run(phase+"-with-reservation", func(t *testing.T) {
			require.Equal(t, phase == "local-complete", cleanupRecoveryPhase(phase, reservation))
		})
	}
}
