package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
)

func recoveryTestSeed(t *testing.T) taskArchiveRecoverySeed {
	t.Helper()
	n, err := NewNamespace("/"+strings.Repeat("n", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	require.Len(t, n.Root(), 512)
	d := strings.Repeat("f", 64)
	id := "11111111-1111-4111-8111-111111111111"
	return taskArchiveRecoverySeed{Task: TaskReference{n.Root(), strings.Repeat("r", 128), id, strings.Repeat("s", 128), 255}, TaskBirth: math.MaxInt64, TaskDigest: d, ClaimID: id, WorkerID: strings.Repeat("w", 128), LeaseID: math.MaxInt64, ClaimRevision: math.MaxInt64, ClaimDigest: d, FinalSyncAttempt: id, FinalSyncRevision: math.MaxInt64, FinalSyncDigest: d, ArchiveCommand: id, ArchiveRevision: math.MaxInt64, ArchiveDigest: d, IssuerRevision: math.MaxInt64, IssuerDigest: d, TicketDigest: d}
}

func recoveryTestManifest(t *testing.T, state string) taskArchiveRecoveryManifest {
	t.Helper()
	id := "11111111-1111-4111-8111-111111111111"
	m := taskArchiveRecoveryManifest{Version: 1, Slot: 63, State: state, StoreID: strings.Repeat("f", 64), LimitsDigest: strings.Repeat("f", 64), Root: taskArchiveRecoveryRoot{math.MaxUint64, math.MaxUint64, math.MaxUint64, math.MaxUint64, math.MaxUint32, math.MaxUint32, id, math.MaxUint64}, Seed: recoveryTestSeed(t), Artifact: &taskArchiveRecoveryArtifact{math.MaxUint64, math.MaxUint64}}
	if state == "pending" {
		return m
	}
	m.Received = &taskArchiveRecoveryReceived{1 << 40, 1000000, strings.Repeat("f", 64)}
	if state == "received" {
		return m
	}
	exclude := make([]string, 256)
	for i := range exclude {
		var p strings.Builder
		for bit := 0; bit < 8; bit++ {
			if i&(1<<bit) != 0 {
				p.WriteRune('\u2028')
			} else {
				p.WriteByte('"')
			}
		}
		p.WriteString(strings.Repeat("\"", 64-p.Len()))
		exclude[i] = p.String()
	}
	w, err := NewWorkspaceIdentity("minio", "storage", strings.Repeat("\"", 256), strings.Repeat("\"", 1023)+"/")
	require.NoError(t, err)
	p, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{w, exclude, true})
	require.NoError(t, err)
	worker := launcher.WorkspaceSyncWorkerReference{Incarnation: id, BootID: id, PID: math.MaxInt32, StartTicks: math.MaxUint64, UID: math.MaxUint32, GID: math.MaxUint32, PIDNamespaceDev: math.MaxUint64, PIDNamespaceIno: math.MaxUint64, MountNamespaceDev: math.MaxUint64, MountNamespaceIno: math.MaxUint64, ExecutableDev: math.MaxUint64, ExecutableIno: math.MaxUint64}
	local := taskWorkspaceSyncLocalAttribution{worker, "/" + strings.Repeat("\"", 4095), math.MaxUint64, math.MaxUint64, math.MaxUint64, 0x01021994, "tmpfs", math.MaxUint32, math.MaxUint32}
	raw, err := encodeOperationMetadata(local, 16384)
	require.NoError(t, err)
	m.Binding = &taskArchiveRecoveryBinding{recoveryPlanWire(p), local, workspaceSyncIntentDigest(raw)}
	return m
}

func TestTaskArchiveRecoveryCodecMaximumCanonical(t *testing.T) {
	for _, state := range []string{"pending", "received", "bound"} {
		t.Run(state, func(t *testing.T) {
			m := recoveryTestManifest(t, state)
			var out, canonical [taskArchiveRecoveryMetadataBytes]byte
			wire, err := encodeTaskArchiveRecovery(m, out[:])
			require.NoError(t, err)
			require.LessOrEqual(t, len(wire), taskArchiveRecoveryWireBound)
			var standard bytes.Buffer
			e := json.NewEncoder(&standard)
			e.SetEscapeHTML(false)
			require.NoError(t, e.Encode(m))
			require.Equal(t, standard.Bytes(), wire)
			fresh, err := decodeTaskArchiveRecovery(wire, canonical[:])
			require.NoError(t, err)
			require.Equal(t, m, fresh)
			t.Logf("legal maximum state=%s wire=%d cap=53121 M=65536", state, len(wire))
			if state == "bound" {
				var content int
				for _, path := range m.Binding.Plan.Exclude {
					content += len(path)
				}
				require.Equal(t, 16384, content)
				require.Len(t, m.Binding.Plan.Exclude, 256)
			}
		})
	}
	t.Run("escape-semantics", func(t *testing.T) {
		m := recoveryTestManifest(t, "bound")
		p, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: m.Binding.PlanWorkspace(), Exclude: []string{"<html>&\"\u2028\u2029"}})
		require.NoError(t, err)
		m.Binding.Plan = recoveryPlanWire(p)
		var out [taskArchiveRecoveryMetadataBytes]byte
		wire, err := encodeTaskArchiveRecovery(m, out[:])
		require.NoError(t, err)
		var standard bytes.Buffer
		e := json.NewEncoder(&standard)
		e.SetEscapeHTML(false)
		require.NoError(t, e.Encode(m))
		require.Equal(t, standard.Bytes(), wire)
	})
}

func (b taskArchiveRecoveryBinding) PlanWorkspace() WorkspaceIdentity {
	p := b.Plan
	return WorkspaceIdentity{p.Provider, p.StorageIdentityHash, p.Bucket, p.Prefix, p.Hash, p.Partition}
}

func TestTaskArchiveRecoveryCodecRejectsAmbiguousWire(t *testing.T) {
	m := recoveryTestManifest(t, "bound")
	var out, canonical [taskArchiveRecoveryMetadataBytes]byte
	wire, err := encodeTaskArchiveRecovery(m, out[:])
	require.NoError(t, err)
	original := string(wire)
	cases := map[string]string{
		"duplicate":           strings.Replace(original, "\"version\":1", "\"version\":1,\"version\":1", 1),
		"unknown":             strings.Replace(original, "\"version\":1", "\"version\":1,\"extra\":1", 1),
		"missing":             strings.Replace(original, "\"version\":1,", "", 1),
		"null":                strings.Replace(original, "\"artifact\":{", "\"unused\":null,\"artifact\":{", 1),
		"noncanonical-number": strings.Replace(original, "\"version\":1", "\"version\":1.0", 1),
		"whitespace":          " " + original,
		"trailing":            original + "{}",
		"oversize":            strings.Repeat(" ", taskArchiveRecoveryWireBound+1),
		"depth":               strings.Repeat("[", 9) + strings.Repeat("]", 9),
		"cardinality":         "{\"exclude\":[" + strings.Repeat("\"\",", 256) + "\"\"]}",
		"null-optional":       strings.Replace(original, "\"binding\":{", "\"binding\":null,\"shadow\":{", 1),
		"escaped-key":         strings.Replace(original, "\"version\"", "\"\\u0076ersion\"", 1),
	}
	for label, wire := range cases {
		t.Run(label, func(t *testing.T) {
			_, err := decodeTaskArchiveRecovery([]byte(wire), canonical[:])
			require.Error(t, err)
		})
	}
	for i, change := range []func(*taskArchiveRecoveryManifest){
		func(m *taskArchiveRecoveryManifest) { m.State = "pending" },
		func(m *taskArchiveRecoveryManifest) { m.Received = nil },
		func(m *taskArchiveRecoveryManifest) { m.Binding.Plan.Hash = strings.Repeat("a", 64) },
		func(m *taskArchiveRecoveryManifest) { m.Binding.TargetDigest = strings.Repeat("a", 64) },
		func(m *taskArchiveRecoveryManifest) {
			m.Binding.Plan.Exclude = append(m.Binding.Plan.Exclude, "overflow")
		},
	} {
		t.Run(fmt.Sprintf("invalid-state-%d", i), func(t *testing.T) {
			copy := recoveryTestManifest(t, "bound")
			change(&copy)
			_, err := encodeTaskArchiveRecovery(copy, canonical[:])
			require.Error(t, err)
		})
	}
}

func TestTaskArchiveRecoveryLimits(t *testing.T) {
	o := recoveryTestOptions(archiveTestDirectory(t), 64)
	i, err := validateTaskArchiveRecovery(&o)
	require.NoError(t, err)
	index, err := taskArchiveIndexBytes(o.Entries)
	require.NoError(t, err)
	require.Equal(t, index+4*taskArchiveRecoveryMetadataBytes, i)
	for _, change := range []func(*TaskWorkspaceArchiveRecoveryOptions){
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.Jobs = 65 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.Jobs = 0 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.JobBytes = 2*taskArchiveRecoveryMetadataBytes + 1535 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.JobBytes = math.MaxUint64 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.Entries = math.MaxUint64 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.JobIndexBytes = i - 1 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.TotalIndexBytes = i - 1 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.TotalBytes = o.JobBytes - 1 },
		func(o *TaskWorkspaceArchiveRecoveryOptions) { o.Directory = "/" + strings.Repeat("a", 4096) },
	} {
		bad := o
		change(&bad)
		_, err := validateTaskArchiveRecovery(&bad)
		require.Error(t, err)
	}
}
