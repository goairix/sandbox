package etcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func localAttributionFixture() *taskWorkspaceSyncLocalAttribution {
	return &taskWorkspaceSyncLocalAttribution{
		Worker: launcher.WorkspaceSyncWorkerReference{Incarnation: "8a7a11c2-9199-4dc9-a5f7-f388a9107745", BootID: "86b3c998-f1b4-44b6-8797-dfba528adbb5", PID: 1, StartTicks: 10, UID: 65532, GID: 65532, PIDNamespaceDev: 4, PIDNamespaceIno: 5, MountNamespaceDev: 4, MountNamespaceIno: 6, ExecutableDev: 7, ExecutableIno: 8},
		Root:   "/work/root", Dev: 4, Ino: 9, MountID: 10, Filesystem: 0x01021994, FilesystemName: "tmpfs", UID: 65532, GID: 65532,
	}
}

func TestTaskWorkspaceSyncLocalRecords(t *testing.T) {
	r := workspaceSyncIntentRecordFixture(t)
	r.Local = localAttributionFixture()
	require.Error(t, r.Validate(), "v1 cannot carry physical attribution")
	w := workspaceSyncIdentity(t, "local", "test-storage", "bucket", "team/project/")
	r.Version, r.Provider, r.WorkspaceHash, r.Reference.Task.Partition = 2, w.provider, w.hash, w.partition
	r.Claim.Task = r.Reference.Task
	value, err := encodeWorkspaceSyncIntent(r, 4096)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(value), CreateRevision: 9, ModRevision: 9, Version: 1}
	decoded, err := decodeWorkspaceSyncIntent(kv, 4096)
	require.NoError(t, err)
	require.Equal(t, r, decoded)
	localWire, err := json.Marshal(r.Local)
	require.NoError(t, err)
	workerWire, err := json.Marshal(r.Local.Worker)
	require.NoError(t, err)
	for _, fault := range []struct{ name, old, replacement string }{
		{"absent-local", `,"local":` + string(localWire), ""},
		{"null-local", `"local":` + string(localWire), `"local":null`},
		{"absent-worker", `"worker":` + string(workerWire) + ",", ""},
		{"null-worker", `"worker":` + string(workerWire), `"worker":null`},
		{"absent-worker-field", `"StartTicks":10,`, ""},
		{"null-worker-field", `"StartTicks":10`, `"StartTicks":null`},
		{"absent-root", `"root":"/work/root",`, ""},
		{"null-root", `"root":"/work/root"`, `"root":null`},
		{"absent-top-field", `"kind":"put_object",`, ""},
		{"null-top-field", `"kind":"put_object"`, `"kind":null`},
	} {
		t.Run("decode-"+fault.name, func(t *testing.T) {
			bad := *kv
			bad.Value = []byte(strings.Replace(value, fault.old, fault.replacement, 1))
			require.NotEqual(t, value, string(bad.Value), "fault must change the canonical wire")
			_, err := decodeWorkspaceSyncIntent(&bad, 4096)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
	for _, fault := range []string{"missing", "provider", "worker", "root", "dev", "inode", "mount", "filesystem", "filesystem-name", "owner", "bound"} {
		t.Run(fault, func(t *testing.T) {
			bad := r
			local := *r.Local
			bad.Local = &local
			switch fault {
			case "missing":
				bad.Local = nil
			case "provider":
				bad.Provider = "s3"
			case "worker":
				local.Worker.StartTicks = 0
			case "root":
				local.Root = "/work/../root"
			case "dev":
				local.Dev = 0
			case "inode":
				local.Ino = 0
			case "mount":
				local.MountID = 0
			case "filesystem":
				local.Filesystem = 0x794c7630
			case "filesystem-name":
				local.FilesystemName = "ext2"
			case "owner":
				local.UID++
			case "bound":
				local.Root = "/" + strings.Repeat("x", 4096)
			}
			require.Error(t, bad.Validate())
		})
	}
	for _, wire := range []string{" " + value, strings.Replace(value, `"mount_id":10`, `"mount_id":10,"mount_id":10`, 1), strings.Replace(value, `"root":"/work/root"`, `"root":"/work/root","extra":0`, 1)} {
		bad := *kv
		bad.Value = []byte(wire)
		_, err := decodeWorkspaceSyncIntent(&bad, 4096)
		require.Error(t, err)
	}
	r.Local.Root = "/" + strings.Repeat("x", 4095)
	_, err = encodeWorkspaceSyncIntent(r, 4096)
	require.Error(t, err, "physical fields consume original B")
	_, err = encodeWorkspaceSyncIntent(r, 16384)
	require.NoError(t, err)
}

func TestTaskWorkspaceSyncLocalUnoriginal(t *testing.T) {
	var s *PreparedTaskWorkspaceSyncSource
	require.Error(t, s.bindLocalPut(context.Background(), nil, nil))
	_, err := s.prepareLocalPut(context.Background(), nil, nil)
	require.Error(t, err)
	_, err = openTaskWorkspaceSyncLocalTarget(context.Background(), &launcher.KernelBoundary{}, TaskWorkspaceSyncPlan{}, "/work/root")
	require.Error(t, err)
	var target *taskWorkspaceSyncLocalTarget
	require.Error(t, target.Close())
}

func TestTaskWorkspaceSyncLocalBodiesRejectedBeforeRPC(t *testing.T) {
	backend, s := syncSourceHostIO(t, nil)
	require.Nil(t, backend.client, "a native request would fail this pure host fixture")
	entry := &taskWorkspaceSyncEntry{origin: s, kind: '0', path: "file"}
	entry.self = entry
	body := &taskWorkspaceSyncBody{source: s, entry: entry, ctx: context.Background()}
	body.self = body
	s.current, s.body = entry, body
	target := &taskWorkspaceSyncLocalTarget{pin: *localAttributionFixture()}
	target.self = target
	body.localTarget = target
	for _, state := range []string{"copy-source", "copy-body", "foreign", "read", "empty-complete", "unknown", "v1", "consumed"} {
		t.Run(state, func(t *testing.T) {
			candidateSource, candidate := s, body
			body.intent = &taskWorkspaceSyncIntentDraft{record: TaskWorkspaceSyncIntentRecord{taskWorkspaceSyncIntentAttribution: taskWorkspaceSyncIntentAttribution{Version: 2}}, revision: 9}
			switch state {
			case "copy-source":
				candidateSource = &PreparedTaskWorkspaceSyncSource{self: s}
			case "copy-body":
				candidate = &taskWorkspaceSyncBody{self: body, source: s, entry: entry, ctx: body.ctx}
			case "foreign":
				candidate = &taskWorkspaceSyncBody{source: &PreparedTaskWorkspaceSyncSource{}, entry: entry, ctx: body.ctx}
				candidate.self = candidate
			case "read":
				body.bytes = 1
			case "empty-complete":
				body.complete = true
			case "v1":
				body.intent.record.Version = 1
				body.intent.acknowledged = true
			case "consumed":
				body.localConsumed = true
			}
			p, err := candidateSource.prepareLocalPut(context.Background(), candidate, target)
			require.Error(t, err)
			require.Nil(t, p)
			body.bytes, body.complete, body.localConsumed = 0, false, false
		})
	}
}
