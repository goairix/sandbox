package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Models in this file exercise canonical records and relationships only. Their
// unsigned terminal bytes, stamps and synthetic KVs never enter a live reader,
// opaque proof, Claim, worker, Stage or native quota writer.
func detachedQuotaReturnCodecModel(t *testing.T) detachedColdRuntimeQuotaReturnRecord {
	t.Helper()
	legacy := quotaReturnRecordFixture(t)
	intent := detachedOwnerReleaseCodecIntent(t)
	claim := legacy.Release.Claim
	release, err := newDetachedOwnerReleaseRecord(intent, 100, 103, claim)
	require.NoError(t, err)
	return detachedColdRuntimeQuotaReturnRecord{
		Version: 2, Domain: "sandbox.cold-runtime-quota-return.v2",
		Namespace: legacy.Namespace, RestoreEpoch: legacy.RestoreEpoch, ProfileDigest: legacy.ProfileDigest,
		Owner: legacy.Owner, Quota: legacy.Quota, Preparation: legacy.Preparation,
		Runtime: release.Runtime, NodeUID: release.NodeUID, NodeBootID: release.NodeBootID, DataGateEpoch: legacy.DataGateEpoch,
		Publication: legacy.Publication, PublicationStamp: legacy.PublicationStamp, Reservation: legacy.Reservation,
		PreparationWork: legacy.PreparationWork, PreparationSettlement: legacy.PreparationSettlement,
		Release: release, ReleaseStamp: legacy.ReleaseStamp, Attempt: legacy.Attempt,
	}
}

func detachedQuotaReturnCodecWire(t *testing.T, record detachedColdRuntimeQuotaReturnRecord) []byte {
	t.Helper()
	wire, err := json.Marshal(record)
	require.NoError(t, err)
	return wire
}

func detachedQuotaReturnCodecKV(record detachedColdRuntimeQuotaReturnRecord, wire []byte) *mvccpb.KeyValue {
	return &mvccpb.KeyValue{
		Key:     []byte(fmt.Sprintf("%sp/%02x/intents/%s/cold-runtime-quota-return", record.Namespace, record.Owner.Partition, record.Owner.IntentID)),
		Value:   wire,
		Version: 1, CreateRevision: 105, ModRevision: 105,
	}
}

func TestDetachedColdRuntimeQuotaReturnCanonicalAndNative(t *testing.T) {
	model := detachedQuotaReturnCodecModel(t)
	want := detachedQuotaReturnCodecWire(t, model)
	encoded, err := encodeDetachedColdRuntimeQuotaReturn(model)
	require.NoError(t, err)
	require.Equal(t, string(want), encoded)
	require.Contains(t, encoded, `"domain":"sandbox.cold-runtime-quota-return.v2"`)
	require.NotContains(t, encoded, `"complete":`)
	decoded, err := decodeDetachedColdRuntimeQuotaReturnValue(want)
	require.NoError(t, err)
	require.Equal(t, model, decoded)
	decoded, err = decodeDetachedColdRuntimeQuotaReturn(detachedQuotaReturnCodecKV(model, want))
	require.NoError(t, err)
	require.Equal(t, model, decoded)

	// The original quota protocol permits a quota partition distinct from the
	// owner. The standalone v2 codec must not invent a stricter equality rule.
	model.Quota.Partition = model.Owner.Partition + 1
	encoded, err = encodeDetachedColdRuntimeQuotaReturn(model)
	require.NoError(t, err)
	decoded, err = decodeDetachedColdRuntimeQuotaReturnValue([]byte(encoded))
	require.NoError(t, err)
	require.Equal(t, model, decoded)
	_, err = decodeColdRuntimeQuotaReturnValue(want)
	require.Error(t, err, "strict v1 must keep rejecting the independent v2 profile")
}

func TestDetachedColdRuntimeQuotaReturnFrozenV1Wire(t *testing.T) {
	// These constant bytes fix the original field order and local Complete
	// representation independently of either encoder or any mutable fixture.
	// Repeated constant fragments only make the canonical JSON readable.
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const namespace = `"namespace":"/codex-test/tasks/cell/"`
	const task = `{` + namespace + `,"restore_epoch":"restore","task_id":"11111111-1111-4111-8111-111111111111","sandbox_id":"sandbox","partition":7}`
	const runtime = `{"id":"pod","uid":"22222222-2222-4222-8222-222222222222","boot_id":"33333333-3333-4333-8333-333333333333"}`
	const node = `"node_uid":"44444444-4444-4444-8444-444444444444","node_boot_id":"55555555-5555-4555-8555-555555555555"`
	const stagePrefix = `{` + namespace + `,"partition":7,"request_id":"request","stage_id":"`
	const attemptTail = `","attempt_id":"11111111-1111-4111-8111-111111111111"`
	const referenceTail = attemptTail + `,"digest":"` + hash + `","restore_epoch":"restore"}`
	const stamp1 = `{"create_revision":1,"mod_revision":1,"sha256":"` + hash + `"}`
	const stamp20 = `{"create_revision":20,"mod_revision":20,"sha256":"` + hash + `"}`
	const stamp30 = `{"create_revision":10,"mod_revision":30,"sha256":"` + hash + `"}`
	const stamp104 = `{"create_revision":104,"mod_revision":104,"sha256":"` + hash + `"}`
	const release = `{"version":1,"domain":"sandbox.task-owner-release.v1","task":` + task +
		`,"task_birth":11,"claim":{"task":` + task + `,"claim_id":"11111111-1111-4111-8111-111111111111","worker_id":"worker","create_revision":99,"lease_id":2}` +
		`,"runtime":` + runtime + `,"workspace_hash":"` + hash + `","generation":1,` + node +
		`,"complete":{"create_revision":80,"mod_revision":80,"sha256":"` + hash + `"}` +
		`,"termination":{"create_revision":100,"mod_revision":103,"sha256":"` + hash + `"}` +
		`,"cleanup_digest":"` + hash + `","deleted":[{"key":"owner","stamp":` + stamp1 +
		`},{"key":"control","stamp":` + stamp1 + `},{"key":"index","stamp":` + stamp1 + `}]}`
	const wire = `{"version":1,"domain":"sandbox.cold-runtime-quota-return.v1",` + namespace +
		`,"restore_epoch":"restore","profile_digest":"` + hash +
		`","owner":{"intent_id":"intent","sandbox_id":"sandbox","workspace_hash":"` + hash + `","request_id":"request","generation":1,"partition":7}` +
		`,"quota":{"kind":"quota","partition":8,"id":0,"epoch":1},"preparation":{"kind":"preparation","partition":0,"id":0,"epoch":1}` +
		`,"runtime":` + runtime + `,` + node + `,"data_gate_epoch":1` +
		`,"publication":` + stagePrefix + `runtime_publish_enrolled_v2` + referenceTail +
		`,"publication_stamp":` + stamp20 + `,"reservation":` + stamp30 + `,"preparation_work":` + stamp30 +
		`,"preparation_settlement":` + stagePrefix + `cold_preparation_settle` + referenceTail +
		`,"release":` + release + `,"release_stamp":` + stamp104 +
		`,"attempt":` + stagePrefix + `cold_runtime_quota_return` + attemptTail + `,"restore_epoch":"restore"}}`

	var original coldRuntimeQuotaReturnRecord
	require.NoError(t, json.Unmarshal([]byte(wire), &original))
	encoded, err := encodeColdRuntimeQuotaReturn(original)
	require.NoError(t, err)
	require.Equal(t, wire, encoded)
	require.NotEqual(t, original.Owner.Partition, original.Quota.Partition)

	decoded, err := decodeColdRuntimeQuotaReturn(&mvccpb.KeyValue{
		Value: []byte(wire), Version: 1, CreateRevision: 105, ModRevision: 105,
	})
	require.NoError(t, err)
	require.Equal(t, original, decoded)
	_, err = decodeDetachedColdRuntimeQuotaReturnValue([]byte(wire))
	require.Error(t, err, "the new profile must never accept original local Complete authority")
}

func TestDetachedColdRuntimeQuotaReturnRejectsForeignRecord(t *testing.T) {
	model := detachedQuotaReturnCodecModel(t)
	for name, change := range map[string]func(*detachedColdRuntimeQuotaReturnRecord){
		"version":               func(r *detachedColdRuntimeQuotaReturnRecord) { r.Version = 1 },
		"domain":                func(r *detachedColdRuntimeQuotaReturnRecord) { r.Domain = "sandbox.cold-runtime-quota-return.v1" },
		"namespace":             func(r *detachedColdRuntimeQuotaReturnRecord) { r.Namespace = "/other/tasks/cell/" },
		"restore":               func(r *detachedColdRuntimeQuotaReturnRecord) { r.RestoreEpoch = "foreign" },
		"profile":               func(r *detachedColdRuntimeQuotaReturnRecord) { r.ProfileDigest = "bad" },
		"owner-intent":          func(r *detachedColdRuntimeQuotaReturnRecord) { r.Owner.IntentID = "" },
		"owner-sandbox":         func(r *detachedColdRuntimeQuotaReturnRecord) { r.Owner.SandboxID = "other" },
		"owner-workspace":       func(r *detachedColdRuntimeQuotaReturnRecord) { r.Owner.WorkspaceHash = strings.Repeat("f", 64) },
		"owner-generation":      func(r *detachedColdRuntimeQuotaReturnRecord) { r.Owner.Generation++ },
		"owner-partition":       func(r *detachedColdRuntimeQuotaReturnRecord) { r.Owner.Partition++ },
		"gate":                  func(r *detachedColdRuntimeQuotaReturnRecord) { r.DataGateEpoch = 0 },
		"runtime":               func(r *detachedColdRuntimeQuotaReturnRecord) { r.Runtime.UID = r.NodeUID },
		"node":                  func(r *detachedColdRuntimeQuotaReturnRecord) { r.NodeUID = r.Runtime.UID },
		"boot":                  func(r *detachedColdRuntimeQuotaReturnRecord) { r.NodeBootID = r.Runtime.BootID },
		"quota-kind":            func(r *detachedColdRuntimeQuotaReturnRecord) { r.Quota.Kind = "preparation" },
		"quota-id":              func(r *detachedColdRuntimeQuotaReturnRecord) { r.Quota.ID = 1000000 },
		"quota-epoch":           func(r *detachedColdRuntimeQuotaReturnRecord) { r.Quota.Epoch = 0 },
		"preparation-kind":      func(r *detachedColdRuntimeQuotaReturnRecord) { r.Preparation.Kind = "quota" },
		"preparation-id":        func(r *detachedColdRuntimeQuotaReturnRecord) { r.Preparation.ID = 32 },
		"preparation-partition": func(r *detachedColdRuntimeQuotaReturnRecord) { r.Preparation.Partition = 1 },
		"preparation-epoch":     func(r *detachedColdRuntimeQuotaReturnRecord) { r.Preparation.Epoch = 0 },
		"attempt-stage":         func(r *detachedColdRuntimeQuotaReturnRecord) { r.Attempt.StageID = "other" },
		"attempt-owner":         func(r *detachedColdRuntimeQuotaReturnRecord) { r.Attempt.RequestID = "other" },
		"attempt-uuid":          func(r *detachedColdRuntimeQuotaReturnRecord) { r.Attempt.AttemptID = "bad" },
		"publication-stage":     func(r *detachedColdRuntimeQuotaReturnRecord) { r.Publication.StageID = "runtime_publish" },
		"publication-request":   func(r *detachedColdRuntimeQuotaReturnRecord) { r.Publication.RequestID = "other" },
		"publication-digest":    func(r *detachedColdRuntimeQuotaReturnRecord) { r.Publication.Digest = "bad" },
		"settlement-stage":      func(r *detachedColdRuntimeQuotaReturnRecord) { r.PreparationSettlement.StageID = "other" },
		"settlement-partition":  func(r *detachedColdRuntimeQuotaReturnRecord) { r.PreparationSettlement.Partition++ },
		"publication-mutable":   func(r *detachedColdRuntimeQuotaReturnRecord) { r.PublicationStamp.ModRevision++ },
		"reservation-order": func(r *detachedColdRuntimeQuotaReturnRecord) {
			r.Reservation.ModRevision = r.PublicationStamp.CreateRevision
			r.PreparationWork.ModRevision = r.Reservation.ModRevision
		},
		"work-stamp":      func(r *detachedColdRuntimeQuotaReturnRecord) { r.PreparationWork.ModRevision++ },
		"release-mutable": func(r *detachedColdRuntimeQuotaReturnRecord) { r.ReleaseStamp.ModRevision++ },
		"release-before-termination": func(r *detachedColdRuntimeQuotaReturnRecord) {
			r.ReleaseStamp.CreateRevision = r.Release.Termination.ModRevision
			r.ReleaseStamp.ModRevision = r.ReleaseStamp.CreateRevision
		},
		"release-before-claim": func(r *detachedColdRuntimeQuotaReturnRecord) {
			r.Release.Claim.CreateRevision = r.ReleaseStamp.CreateRevision
		},
		"nested-version":        func(r *detachedColdRuntimeQuotaReturnRecord) { r.Release.Version = 1 },
		"nested-domain":         func(r *detachedColdRuntimeQuotaReturnRecord) { r.Release.Domain = "sandbox.task-owner-release.v1" },
		"nested-effect-profile": func(r *detachedColdRuntimeQuotaReturnRecord) { r.Release.DetachedEffects.Profile = "local" },
		"nested-fact-stamp":     func(r *detachedColdRuntimeQuotaReturnRecord) { r.Release.DetachedEffects.Settlement.SHA256 = "bad" },
		"nested-deleted-key":    func(r *detachedColdRuntimeQuotaReturnRecord) { r.Release.Deleted[0].Key += "foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := model
			change(&bad)
			_, err := encodeDetachedColdRuntimeQuotaReturn(bad)
			require.Error(t, err)
			_, err = decodeDetachedColdRuntimeQuotaReturnValue(detachedQuotaReturnCodecWire(t, bad))
			require.Error(t, err)
		})
	}
}

func TestDetachedColdRuntimeQuotaReturnStrictCanonical(t *testing.T) {
	model := detachedQuotaReturnCodecModel(t)
	wire := string(detachedQuotaReturnCodecWire(t, model))
	for name, bad := range map[string]string{
		"whitespace":            " " + wire,
		"trailing":              wire + "{}",
		"duplicate":             strings.Replace(wire, `"version":2`, `"version":2,"version":2`, 1),
		"unknown":               strings.Replace(wire, `"version":2`, `"version":2,"unknown":true`, 1),
		"fraction":              strings.Replace(wire, `"version":2`, `"version":2.0`, 1),
		"missing-version":       strings.Replace(wire, `"version":2,`, "", 1),
		"missing-profile":       strings.Replace(wire, `,"profile_digest":"`+model.ProfileDigest+`"`, "", 1),
		"nested-local-complete": strings.Replace(wire, `"detached_effects":`, `"complete":{"create_revision":1,"mod_revision":1,"sha256":"`+strings.Repeat("a", 64)+`"},"detached_effects":`, 1),
		"nested-local-archive":  strings.Replace(wire, `"detached_effects":`, `"archive":{},"detached_effects":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeDetachedColdRuntimeQuotaReturnValue([]byte(bad))
			require.Error(t, err)
		})
	}
	for _, field := range []struct {
		name  string
		value any
	}{
		{"owner", model.Owner}, {"quota", model.Quota}, {"preparation", model.Preparation},
		{"runtime", model.Runtime}, {"publication", model.Publication}, {"release", model.Release},
		{"publication_stamp", model.PublicationStamp}, {"release_stamp", model.ReleaseStamp}, {"attempt", model.Attempt},
	} {
		t.Run("null-"+field.name, func(t *testing.T) {
			value, err := json.Marshal(field.value)
			require.NoError(t, err)
			original := `"` + field.name + `":` + string(value)
			require.Contains(t, wire, original)
			bad := strings.Replace(wire, original, `"`+field.name+`":null`, 1)
			_, err = decodeDetachedColdRuntimeQuotaReturnValue([]byte(bad))
			require.Error(t, err)
		})
	}
}

func TestDetachedColdRuntimeQuotaReturnNativeEnvelope(t *testing.T) {
	model := detachedQuotaReturnCodecModel(t)
	kv := detachedQuotaReturnCodecKV(model, detachedQuotaReturnCodecWire(t, model))
	_, err := decodeDetachedColdRuntimeQuotaReturn(kv)
	require.NoError(t, err)
	for name, change := range map[string]func(*mvccpb.KeyValue){
		"leased":          func(k *mvccpb.KeyValue) { k.Lease = 1 },
		"missing-version": func(k *mvccpb.KeyValue) { k.Version = 0 },
		"updated-version": func(k *mvccpb.KeyValue) { k.Version = 2 },
		"mutated-mod":     func(k *mvccpb.KeyValue) { k.ModRevision++ },
		"inverted-mod":    func(k *mvccpb.KeyValue) { k.ModRevision-- },
		"missing-birth":   func(k *mvccpb.KeyValue) { k.CreateRevision = 0 },
		"not-after-release": func(k *mvccpb.KeyValue) {
			k.CreateRevision, k.ModRevision = model.ReleaseStamp.CreateRevision, model.ReleaseStamp.CreateRevision
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := *kv
			change(&bad)
			_, err := decodeDetachedColdRuntimeQuotaReturn(&bad)
			require.Error(t, err)
		})
	}
	_, err = decodeDetachedColdRuntimeQuotaReturn(nil)
	require.Error(t, err)
}

// Build one simultaneously maximal, constructor-valid release and outer model.
// No sum of unrelated optimistic field budgets substitutes for these bytes.
func detachedQuotaReturnMaximumModel(t *testing.T) detachedColdRuntimeQuotaReturnRecord {
	t.Helper()
	model := detachedQuotaReturnCodecModel(t)
	r := detachedOwnerReleaseCodecIntent(t)
	namespace, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	require.Len(t, namespace.Root(), 512)

	r.Current.Namespace, r.Current.RestoreEpoch, r.Current.SandboxID = namespace.Root(), strings.Repeat("e", 128), strings.Repeat("s", 128)
	r.Current.WorkspaceHash, r.Partition = strings.Repeat("f", 64), 255
	r.Current.WorkerID = strings.Repeat("w", 128)
	r.Current.Runtime = p.RuntimeReference{
		ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128),
	}
	r.TaskBirth, r.Current.ControlRevision, r.Current.ClaimCreateRevision = math.MaxInt64-6, math.MaxInt64-6, math.MaxInt64-5
	r.Current.LeaseID, r.Current.Generation, r.Current.DataGateEpoch = math.MaxInt64, math.MaxInt64, math.MaxInt64
	r.TerminalProof = make([]byte, 4096)
	stamp := p.RecordStamp{CreateRevision: math.MaxInt64 - 8, ModRevision: math.MaxInt64 - 7, SHA256: strings.Repeat("f", 64)}
	r.Authority.Owner, r.Authority.Control, r.Authority.RuntimeIndex, r.DetachedEffects.Settlement = stamp, stamp, stamp, stamp
	claim := taskCloseClaimWire{
		Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: strings.Repeat("w", 128),
		CreateRevision: math.MaxInt64 - 2, LeaseID: math.MaxInt64,
	}
	model.Release, err = newDetachedOwnerReleaseRecord(r, math.MaxInt64-4, math.MaxInt64-3, claim)
	require.NoError(t, err)
	nested, err := encodeDetachedOwnerRelease(model.Release)
	require.NoError(t, err)
	require.Len(t, nested, 7643, "the accepted worst nested release must coexist with maximal outer dimensions")

	model.Namespace, model.RestoreEpoch = namespace.Root(), r.Current.RestoreEpoch
	// The seal does not carry the creation-intent ID. This maximal codec-only
	// model uses the outer field's full domain limit; it does not claim that a
	// future source-history proof could authenticate this particular identity.
	model.Owner = ColdSlotOwner{
		IntentID: strings.Repeat("i", 128), SandboxID: r.Current.SandboxID,
		WorkspaceHash: r.Current.WorkspaceHash, RequestID: strings.Repeat("r", 128),
		Generation: math.MaxInt64, Partition: 255,
	}
	model.Quota = ColdSlotReference{Kind: "quota", Partition: 255, ID: 999999, Epoch: math.MaxUint64}
	model.Preparation = ColdSlotReference{Kind: "preparation", ID: 31, Epoch: math.MaxUint64}
	model.Runtime, model.NodeUID, model.NodeBootID, model.DataGateEpoch = model.Release.Runtime, model.Release.NodeUID, model.Release.NodeBootID, math.MaxInt64

	model.Attempt = StageAttemptLocator{
		Namespace: model.Namespace, RestoreEpoch: model.RestoreEpoch, Partition: 255,
		RequestID: model.Owner.RequestID, StageID: "cold_runtime_quota_return", AttemptID: claim.ClaimID,
	}
	require.NoError(t, model.Attempt.Validate())
	publication, settlement := model.Attempt, model.Attempt
	publication.StageID, settlement.StageID = enrolledPublicationStage, "cold_preparation_settle"
	require.NoError(t, publication.Validate())
	require.NoError(t, settlement.Validate())
	model.Publication, model.PreparationSettlement = publication.reference(strings.Repeat("c", 64)), settlement.reference(strings.Repeat("d", 64))

	model.PublicationStamp = p.RecordStamp{CreateRevision: math.MaxInt64 - 12, ModRevision: math.MaxInt64 - 12, SHA256: strings.Repeat("a", 64)}
	model.Reservation = p.RecordStamp{CreateRevision: math.MaxInt64 - 14, ModRevision: math.MaxInt64 - 10, SHA256: strings.Repeat("b", 64)}
	model.PreparationWork = model.Reservation
	model.ReleaseStamp = p.RecordStamp{CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64 - 1, SHA256: strings.Repeat("e", 64)}
	return model
}

func TestDetachedColdRuntimeQuotaReturnMaximumAggregateAndCap(t *testing.T) {
	model := detachedQuotaReturnMaximumModel(t)
	raw := detachedQuotaReturnCodecWire(t, model)
	require.LessOrEqual(t, len(raw), 16384, "one aggregate valid model, not separately maximized leaves")
	require.Equal(t, 16<<10, detachedColdRuntimeQuotaReturnBytes)
	t.Logf("codec-only aggregate maximal v2 return=%d bytes including accepted nested7643, root512, outer128 and maximal scalars", len(raw))

	wire, err := encodeDetachedColdRuntimeQuotaReturn(model)
	require.NoError(t, err)
	require.Equal(t, string(raw), wire)
	_, err = decodeDetachedColdRuntimeQuotaReturnValue(raw)
	require.NoError(t, err)
	kv := detachedQuotaReturnCodecKV(model, raw)
	kv.CreateRevision, kv.ModRevision = math.MaxInt64, math.MaxInt64
	_, err = decodeDetachedColdRuntimeQuotaReturn(kv)
	require.NoError(t, err)

	// Per-record protobuf accounting only: use the full aggregate value and its
	// derived return key, immutable KV envelope and maximal header scalars. This
	// is neither a transaction projection nor authenticated native evidence.
	put := &pb.PutRequest{Key: kv.Key, Value: []byte(wire)}
	header := &pb.ResponseHeader{
		ClusterId: math.MaxUint64, MemberId: math.MaxUint64,
		Revision: math.MaxInt64, RaftTerm: math.MaxUint64,
	}
	ranged := &pb.RangeResponse{Header: header, Kvs: []*mvccpb.KeyValue{kv}, Count: 1}
	require.LessOrEqual(t, put.Size(), 262144)
	require.LessOrEqual(t, ranged.Size(), 262144)
	t.Logf("per-record only: quota v2 key=%d value=%d PutRequest=%d RangeResponse=%d bytes, transport cap=262144", len(kv.Key), len(wire), put.Size(), ranged.Size())

	// Whitespace and oversized fields are independently invalid canonical data;
	// these exact-length inputs additionally exercise the hard value-size gate.
	over := append(bytes.Clone(raw), bytes.Repeat([]byte{' '}, 16385-len(raw))...)
	require.Len(t, over, 16385)
	_, err = decodeDetachedColdRuntimeQuotaReturnValue(over)
	require.Error(t, err)
	kv.Value = over
	_, err = decodeDetachedColdRuntimeQuotaReturn(kv)
	require.Error(t, err)
	tooWide := model
	tooWide.Release.Runtime.ID = strings.Repeat("<", 16385)
	tooWide.Runtime = tooWide.Release.Runtime
	_, err = encodeDetachedColdRuntimeQuotaReturn(tooWide)
	require.Error(t, err)
}
