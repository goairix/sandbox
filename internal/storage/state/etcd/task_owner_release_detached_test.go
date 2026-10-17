package etcd

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This is a codec model, including deliberately unsigned terminal bytes. No
// test in this file supplies this model to a live capability or native writer.
func detachedOwnerReleaseCodecIntent(t *testing.T) p.TaskRuntimeTerminationIntent {
	t.Helper()
	r := localReleaseTerminationFixture(t)
	r.Version, r.Phase = 2, "cleanup-complete"
	r.Authority.Archive, r.Authority.LocalComplete = p.RecordStamp{}, p.RecordStamp{}
	r.DetachedEffects = &p.DetachedRuntimeEffectBindings{Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("b", 64), Settlement: p.RecordStamp{CreateRevision: 12, ModRevision: 12, SHA256: strings.Repeat("c", 64)}}
	r.TerminalProof, r.CleanupDigest = []byte("unsigned-codec-only"), strings.Repeat("d", 64)
	_, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	return r
}

// Expected fields are assembled independently of the new constructor. The
// original v2 key codec supplies locators only; it authenticates no native KV.
func detachedOwnerReleaseCodecModel(t *testing.T, r p.TaskRuntimeTerminationIntent, birth, mod int64, claim taskCloseClaimWire) (detachedOwnerReleaseRecord, *mvccpb.KeyValue) {
	t.Helper()
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(r)
	require.NoError(t, err)
	expected := detachedOwnerReleaseRecord{Version: 2, Domain: "sandbox.task-owner-release.v2", Task: claim.Task, TaskBirth: r.TaskBirth, Claim: claim,
		Runtime: RuntimeReference(r.Current.Runtime), WorkspaceHash: r.Current.WorkspaceHash, Generation: r.Current.Generation, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
		Termination: p.RecordStamp{CreateRevision: birth, ModRevision: mod, SHA256: workspaceSyncIntentDigest(string(wire))}, CleanupDigest: r.CleanupDigest, DetachedEffects: *r.DetachedEffects,
		Deleted: [3]localOwnerReleasedKey{{keys.Owner, r.Authority.Owner}, {keys.Control, r.Authority.Control}, {keys.RuntimeIndex, r.Authority.RuntimeIndex}},
	}
	return expected, &mvccpb.KeyValue{Key: []byte(keys.Intent), Value: wire, Version: 3, CreateRevision: birth, ModRevision: mod}
}

func TestDetachedOwnerReleaseRecordAndHistoricalClaim(t *testing.T) {
	r := detachedOwnerReleaseCodecIntent(t)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: "11111111-1111-4111-8111-111111111111", WorkerID: "new-release-worker", CreateRevision: 110, LeaseID: 99}
	expected, term := detachedOwnerReleaseCodecModel(t, r, 100, 103, claim)
	require.NotEqual(t, r.Current.ClaimID, claim.ClaimID)
	require.Greater(t, claim.CreateRevision, r.Current.ClaimCreateRevision, "historical Current never becomes today's live permission")
	got, err := newDetachedOwnerReleaseRecord(r, term.CreateRevision, term.ModRevision, claim)
	require.NoError(t, err)
	require.Equal(t, expected, got)
	wire, err := encodeDetachedOwnerRelease(got)
	require.NoError(t, err)
	raw, err := json.Marshal(expected)
	require.NoError(t, err)
	require.Equal(t, string(raw), wire)
	kv := &mvccpb.KeyValue{Key: []byte(strings.TrimSuffix(string(term.Key), "runtime-termination") + "owner-release"), Value: []byte(wire), Version: 1, CreateRevision: 111, ModRevision: 111}
	decoded, err := decodeDetachedOwnerRelease(kv)
	require.NoError(t, err)
	require.Equal(t, expected, decoded)
	require.NoError(t, matchDetachedOwnerReleaseHistory(decoded, term))
	_, err = decodeLocalOwnerRelease(kv)
	require.Error(t, err, "the public v1 codec must reject the v2 seal")
	_, err = newLocalOwnerReleaseRecord(r, term.CreateRevision, term.ModRevision, claim)
	require.Error(t, err, "the public v1 constructor must reject detached authority")
}

func TestDetachedOwnerReleaseStrictCodec(t *testing.T) {
	r := detachedOwnerReleaseCodecIntent(t)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: "release", CreateRevision: 90, LeaseID: 99}
	model, term := detachedOwnerReleaseCodecModel(t, r, 100, 103, claim)
	raw, err := json.Marshal(model)
	require.NoError(t, err)
	key := strings.TrimSuffix(string(term.Key), "runtime-termination") + "owner-release"
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: raw, Version: 1, CreateRevision: 104, ModRevision: 104}
	_, err = decodeDetachedOwnerRelease(kv)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*detachedOwnerReleaseRecord){
		"v1":                 func(v *detachedOwnerReleaseRecord) { v.Version = 1 },
		"domain":             func(v *detachedOwnerReleaseRecord) { v.Domain = "sandbox.task-owner-release.v1" },
		"task-birth":         func(v *detachedOwnerReleaseRecord) { v.TaskBirth = 0 },
		"foreign-claim-task": func(v *detachedOwnerReleaseRecord) { v.Claim.Task.SandboxID = "foreign" },
		"claim-before-task":  func(v *detachedOwnerReleaseRecord) { v.Claim.CreateRevision = v.TaskBirth },
		"claim-lease":        func(v *detachedOwnerReleaseRecord) { v.Claim.LeaseID = 0 },
		"profile":            func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.Profile = "local" },
		"effects-version":    func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.Version = 2 },
		"contract":           func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.ContractDigest = "bad" },
		"settlement-birth":   func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.Settlement.CreateRevision = 0 },
		"settlement-mod":     func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.Settlement.ModRevision = 0 },
		"termination-hash":   func(v *detachedOwnerReleaseRecord) { v.Termination.SHA256 = "BAD" },
		"deleted-stamp":      func(v *detachedOwnerReleaseRecord) { v.Deleted[1].Stamp.ModRevision = 0 },
		"foreign-owner":      func(v *detachedOwnerReleaseRecord) { v.Deleted[0].Key += "-foreign" },
		"foreign-control":    func(v *detachedOwnerReleaseRecord) { v.Deleted[1].Key += "-foreign" },
		"foreign-index":      func(v *detachedOwnerReleaseRecord) { v.Deleted[2].Key += "-foreign" },
		"swapped-deletes":    func(v *detachedOwnerReleaseRecord) { v.Deleted[0], v.Deleted[1] = v.Deleted[1], v.Deleted[0] },
	} {
		t.Run(name, func(t *testing.T) {
			bad := model
			mutate(&bad)
			_, err := encodeDetachedOwnerRelease(bad)
			require.Error(t, err)
			wire, err := json.Marshal(bad)
			require.NoError(t, err)
			copy := *kv
			copy.Value = wire
			_, err = decodeDetachedOwnerRelease(&copy)
			require.Error(t, err)
		})
	}
	for _, bad := range []string{" " + string(raw), strings.Replace(string(raw), `"version":2`, `"version":2,"version":2`, 1), strings.Replace(string(raw), `"version":2`, `"version":2,"unknown":true`, 1), strings.Repeat("x", 8193)} {
		copy := *kv
		copy.Value = []byte(bad)
		_, err := decodeDetachedOwnerRelease(&copy)
		require.Error(t, err)
	}
	for _, name := range []string{"claim", "detached_effects", "deleted", "task", "termination"} {
		var fields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &fields))
		fields[name] = json.RawMessage("null")
		bad, err := json.Marshal(fields)
		require.NoError(t, err)
		copy := *kv
		copy.Value = bad
		_, err = decodeDetachedOwnerRelease(&copy)
		require.Error(t, err, "null %s must fail independently of unknown fields", name)
	}
	for _, mutate := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 1 }, func(k *mvccpb.KeyValue) { k.Version = 2 }, func(k *mvccpb.KeyValue) { k.ModRevision++ }, func(k *mvccpb.KeyValue) { k.CreateRevision, k.ModRevision = 103, 103 }, func(k *mvccpb.KeyValue) { k.Key = []byte(key + "-foreign") }} {
		copy := *kv
		mutate(&copy)
		_, err := decodeDetachedOwnerRelease(&copy)
		require.Error(t, err)
	}
}

func TestDetachedOwnerReleaseHistoryBindingRejectsForeignVariant(t *testing.T) {
	r := detachedOwnerReleaseCodecIntent(t)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: "release", CreateRevision: 90, LeaseID: 99}
	model, term := detachedOwnerReleaseCodecModel(t, r, 100, 103, claim)
	require.NoError(t, matchDetachedOwnerReleaseHistory(model, term), "pure attribution only; no terminal signature is authenticated here")
	for _, mutate := range []func(*detachedOwnerReleaseRecord){
		func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.ContractDigest = strings.Repeat("e", 64) },
		func(v *detachedOwnerReleaseRecord) { v.DetachedEffects.Settlement.SHA256 = strings.Repeat("e", 64) },
		func(v *detachedOwnerReleaseRecord) { v.CleanupDigest = strings.Repeat("e", 64) },
		func(v *detachedOwnerReleaseRecord) { v.Deleted[0].Stamp.SHA256 = strings.Repeat("e", 64) },
		func(v *detachedOwnerReleaseRecord) { v.Termination.ModRevision++ },
	} {
		bad := model
		mutate(&bad)
		_, err := encodeDetachedOwnerRelease(bad)
		require.NoError(t, err, "foreign but codec-valid history must fail the binding, not merely its schema")
		require.Error(t, matchDetachedOwnerReleaseHistory(bad, term))
	}
	for _, phase := range []string{"reserved", "terminated"} {
		bad := r
		bad.Phase, bad.CleanupDigest = phase, ""
		if phase == "reserved" {
			bad.TerminalProof = nil
		}
		wire, err := p.EncodeTaskRuntimeTerminationIntent(bad)
		require.NoError(t, err)
		copy := *term
		copy.Value = wire
		require.Error(t, matchDetachedOwnerReleaseHistory(model, &copy))
		_, err = newDetachedOwnerReleaseRecord(bad, term.CreateRevision, term.ModRevision, claim)
		require.Error(t, err)
	}
	local := r
	local.Version, local.DetachedEffects = 1, nil
	local.Authority.Archive, local.Authority.LocalComplete = local.Authority.Task, local.Authority.Task
	wire, err := p.EncodeTaskRuntimeTerminationIntent(local)
	require.NoError(t, err)
	copy := *term
	copy.Value = wire
	require.Error(t, matchDetachedOwnerReleaseHistory(model, &copy))
}

func TestDetachedOwnerReleaseMaximumCodecWire(t *testing.T) {
	r := detachedOwnerReleaseCodecIntent(t)
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	r.Current.Namespace, r.Current.RestoreEpoch, r.Current.SandboxID = n.Root(), strings.Repeat("e", 128), strings.Repeat("s", 128)
	r.Current.WorkspaceHash, r.Partition = strings.Repeat("f", 64), 255
	r.Current.WorkerID, r.Current.Runtime = strings.Repeat("w", 128), p.RuntimeReference{ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128)}
	r.TaskBirth, r.Current.ControlRevision, r.Current.ClaimCreateRevision = math.MaxInt64-6, math.MaxInt64-6, math.MaxInt64-5
	r.Current.LeaseID, r.Current.Generation, r.Current.DataGateEpoch = math.MaxInt64, math.MaxInt64, math.MaxInt64
	r.TerminalProof = make([]byte, 4096)
	s := p.RecordStamp{CreateRevision: math.MaxInt64 - 8, ModRevision: math.MaxInt64 - 7, SHA256: strings.Repeat("f", 64)}
	r.Authority.Owner, r.Authority.Control, r.Authority.RuntimeIndex, r.DetachedEffects.Settlement = s, s, s, s
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: strings.Repeat("w", 128), CreateRevision: math.MaxInt64 - 2, LeaseID: math.MaxInt64}
	model, _ := detachedOwnerReleaseCodecModel(t, r, math.MaxInt64-4, math.MaxInt64-3, claim)
	// Independent upper bound: ASCII root512 twice, restore/sandbox128 twice,
	// runtime3*128 bytes each escaping to six JSON bytes, fixed UUID/hash widths,
	// all int64 fields19 digits, partition3 digits and exact derived delete keys
	// (598/654/592 bytes). Full compact JSON is at most7643 bytes, below8KiB.
	wire, err := encodeDetachedOwnerRelease(model)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 7643)
	require.LessOrEqual(t, len(wire), 8192)
	kv := &mvccpb.KeyValue{Key: []byte(n.Root() + "p/ff/tasks/" + claim.Task.TaskID + "/owner-release"), Value: []byte(wire), Version: 1, CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64 - 1}
	_, err = decodeDetachedOwnerRelease(kv)
	require.NoError(t, err)
	t.Logf("codec-only maximal seal=%d bytes; no signature/native authority claimed", len(wire))
}
