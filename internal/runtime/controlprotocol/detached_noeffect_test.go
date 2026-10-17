package controlprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These are protocol dimensions, not native history or a runnable cleanup
// owner. The etcd adapter must independently authenticate Task/Claim/Run/KVs.
func detachedNoEffectWireFixture(t *testing.T) DetachedNoEffectSettlement {
	t.Helper()
	old := nodeTerminationIntentFixture(t)
	q := old.Current
	q.ControlRevision, q.ClaimCreateRevision, q.LeaseID = 11, 12, 13
	stamp := func(revision int64) RecordStamp {
		return RecordStamp{CreateRevision: revision, ModRevision: revision, SHA256: strings.Repeat("a", 64)}
	}
	execution := []byte(`{"codec_fixture":"not an execution capability"}`)
	hash := sha256.Sum256(execution)
	r := DetachedNoEffectSettlement{
		Version: 1, Domain: "sandbox.detached-noeffect.v1", Profile: "detached_enrolled_plain",
		QuiescenceContext: q, Partition: old.Partition, TaskBirth: 11,
		CreationIntentID: old.CreationIntentID,
		Incarnation:      "6c0e9162-e719-55cb-9bc9-946440bd7eae", RunID: "1c0e9162-e719-45cb-9bc9-946440bd7eae", Slot: 31,
		WorkerDigest:       strings.Repeat("b", 64),
		WriterClaim:        DetachedNoEffectClaim{ClaimID: "2c0e9162-e719-45cb-9bc9-946440bd7eae", WorkerID: "writer", CreateRevision: 18, LeaseID: 19},
		FinalSyncClaim:     DetachedNoEffectClaim{ClaimID: "3c0e9162-e719-45cb-9bc9-946440bd7eae", WorkerID: "old-none", CreateRevision: 14, LeaseID: 15},
		FinalSyncAttemptID: "4c0e9162-e719-45cb-9bc9-946440bd7eae", AttemptID: "5c0e9162-e719-45cb-9bc9-946440bd7eae",
		ContractDigest: strings.Repeat("c", 64), ReceiptDigest: strings.Repeat("d", 64), EmptyRevision: 19,
		Task:     RecordStamp{CreateRevision: 11, ModRevision: 11, SHA256: q.TaskDigest},
		Snapshot: stamp(2), CreationIntent: stamp(3), Publication: stamp(4),
		Quiescence: stamp(16), QuiescenceReceipt: stamp(16), QuiescenceReservation: stamp(15), FinalSync: stamp(17),
		Execution: RecordStamp{CreateRevision: 13, ModRevision: 20, SHA256: hex.EncodeToString(hash[:])}, Occupation: stamp(14),
		QuiescenceStage: old.Authority.QuiescenceStage, ExecutionValue: execution,
	}
	var err error
	r.BindingDigest, err = DetachedNoEffectBindingDigest(r)
	require.NoError(t, err)
	return r
}

func TestDetachedNoEffectSharedWireRoundTrip(t *testing.T) {
	r := detachedNoEffectWireFixture(t)
	wire, err := EncodeDetachedNoEffectSettlement(r)
	require.NoError(t, err)
	got, err := DecodeDetachedNoEffectSettlement(wire)
	require.NoError(t, err)
	require.Equal(t, r, got)
	require.NotEqual(t, got.WriterClaim, got.FinalSyncClaim, "new Run claim must not overwrite the original none Claim")
	owned := bytes.Clone(got.ExecutionValue)
	clear(wire)
	require.Equal(t, owned, got.ExecutionValue, "decoded E bytes must not alias the native response buffer")
}

func TestDetachedNoEffectSharedWireStrict(t *testing.T) {
	r := detachedNoEffectWireFixture(t)
	wire, err := EncodeDetachedNoEffectSettlement(r)
	require.NoError(t, err)
	claimWire, err := json.Marshal(r.WriterClaim)
	require.NoError(t, err)
	for name, bad := range map[string][]byte{
		"duplicate":   bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":     bytes.Replace(wire, []byte(`"version":1`), []byte(`"version":1,"permission":true`), 1),
		"missing":     bytes.Replace(wire, []byte(`"slot":31,`), nil, 1),
		"null":        bytes.Replace(wire, []byte(`"slot":31`), []byte(`"slot":null`), 1),
		"nested null": bytes.Replace(wire, append([]byte(`"writer_claim":`), claimWire...), []byte(`"writer_claim":null`), 1),
		"whitespace":  append([]byte(" "), wire...),
		"oversize":    bytes.Repeat([]byte{' '}, 32769),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeDetachedNoEffectSettlement(bad)
			require.Error(t, err)
		})
	}

	for name, mutate := range map[string]func(*DetachedNoEffectSettlement){
		"version":                   func(r *DetachedNoEffectSettlement) { r.Version++ },
		"domain":                    func(r *DetachedNoEffectSettlement) { r.Domain = "local" },
		"profile":                   func(r *DetachedNoEffectSettlement) { r.Profile = "local" },
		"slot":                      func(r *DetachedNoEffectSettlement) { r.Slot = 32 },
		"missing writer":            func(r *DetachedNoEffectSettlement) { r.WriterClaim.ClaimID = "" },
		"missing none claim":        func(r *DetachedNoEffectSettlement) { r.FinalSyncClaim.LeaseID = 0 },
		"missing none attempt":      func(r *DetachedNoEffectSettlement) { r.FinalSyncAttemptID = "" },
		"missing attempt":           func(r *DetachedNoEffectSettlement) { r.AttemptID = "" },
		"wrong task birth":          func(r *DetachedNoEffectSettlement) { r.TaskBirth++ },
		"missing source stamp":      func(r *DetachedNoEffectSettlement) { r.Publication = RecordStamp{} },
		"missing empty observation": func(r *DetachedNoEffectSettlement) { r.EmptyRevision = 0 },
		"wrong E digest":            func(r *DetachedNoEffectSettlement) { r.Execution.SHA256 = strings.Repeat("0", 64) },
		"invalid E JSON":            func(r *DetachedNoEffectSettlement) { r.ExecutionValue = []byte(`{"broken":`) },
		"huge E":                    func(r *DetachedNoEffectSettlement) { r.ExecutionValue = bytes.Repeat([]byte{' '}, 4097) },
		"changed contract":          func(r *DetachedNoEffectSettlement) { r.ContractDigest = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r
			mutate(&changed)
			if name == "invalid E JSON" || name == "huge E" {
				hash := sha256.Sum256(changed.ExecutionValue)
				changed.Execution.SHA256 = hex.EncodeToString(hash[:])
			}
			if name != "changed contract" {
				// Recompute unrelated hashes so the semantic validator itself
				// must reject malformed structure rather than an old digest.
				changed.BindingDigest, err = DetachedNoEffectBindingDigest(changed)
				require.NoError(t, err)
			}
			_, err := EncodeDetachedNoEffectSettlement(changed)
			require.Error(t, err)
			bad, err := json.Marshal(changed)
			require.NoError(t, err)
			_, err = DecodeDetachedNoEffectSettlement(bad)
			require.Error(t, err)
		})
	}

	// A new contract with a new hash is legal wire attribution. Only actual
	// enrolled history and the native reader can authenticate the plain image.
	r.ContractDigest = strings.Repeat("f", 64)
	r.BindingDigest, err = DetachedNoEffectBindingDigest(r)
	require.NoError(t, err)
	_, err = EncodeDetachedNoEffectSettlement(r)
	require.NoError(t, err)
}

func TestDetachedNoEffectBindingSurvivesOnlyProgressFenceChange(t *testing.T) {
	r := detachedNoEffectWireFixture(t)
	before := r.BindingDigest
	r.Execution.ModRevision++
	r.ExecutionValue = []byte(`{"codec_fixture":"the new pending digest is stored here"}`)
	hash := sha256.Sum256(r.ExecutionValue)
	r.Execution.SHA256 = hex.EncodeToString(hash[:])
	after, err := DetachedNoEffectBindingDigest(r)
	require.NoError(t, err)
	require.Equal(t, before, after, "Pending/E.mod must not create a digest cycle")
	r.WriterClaim.ClaimID = "7c0e9162-e719-45cb-9bc9-946440bd7eae"
	after, err = DetachedNoEffectBindingDigest(r)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "another writer cannot reuse the old pending digest")
}

func TestDetachedNoEffectBindingDomainAndFraming(t *testing.T) {
	r := detachedNoEffectWireFixture(t)
	wire, err := json.Marshal(r)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &fields))
	// The literal field order and domain are the independent digest oracle.
	// No production projection builder supplies its own expected framing.
	names := []string{"version", "domain", "profile", "quiescence_context", "partition", "task_birth", "creation_intent_id", "incarnation", "run_id", "slot", "worker_digest", "writer_claim", "final_sync_claim", "final_sync_attempt_id", "attempt_id", "contract_digest", "receipt_digest", "empty_revision", "task", "snapshot", "creation_intent", "publication", "quiescence", "quiescence_receipt", "quiescence_reservation", "final_sync", "occupation", "quiescence_stage"}
	var expected bytes.Buffer
	expected.WriteString("sandbox.detached-noeffect-binding.v1\x00{")
	for i, name := range names {
		if i != 0 {
			expected.WriteByte(',')
		}
		key, err := json.Marshal(name)
		require.NoError(t, err)
		expected.Write(key)
		expected.WriteByte(':')
		require.NotEmpty(t, fields[name])
		expected.Write(fields[name])
	}
	expected.WriteByte('}')
	hash := sha256.Sum256(expected.Bytes())
	require.Equal(t, hex.EncodeToString(hash[:]), r.BindingDigest)
}

func TestDetachedNoEffectBindingCoversEveryNonExecutionScalar(t *testing.T) {
	original := detachedNoEffectWireFixture(t)
	var visit func(reflect.Value, string)
	visit = func(value reflect.Value, path string) {
		for i := 0; i < value.NumField(); i++ {
			field := value.Type().Field(i)
			name := path + field.Name
			if path == "" && (field.Name == "Execution" || field.Name == "ExecutionValue" || field.Name == "BindingDigest") {
				continue
			}
			if value.Field(i).Kind() == reflect.Struct {
				visit(value.Field(i), name+".")
				continue
			}
			t.Run(name, func(t *testing.T) {
				changed := original
				v := reflect.ValueOf(&changed).Elem()
				for _, part := range strings.Split(name, ".") {
					v = v.FieldByName(part)
				}
				switch v.Kind() {
				case reflect.String:
					v.SetString(v.String() + "x")
				case reflect.Int64:
					v.SetInt(v.Int() + 1)
				case reflect.Uint8, reflect.Uint32:
					v.SetUint(v.Uint() + 1)
				default:
					t.Fatalf("uncovered binding scalar %s", v.Kind())
				}
				// Digest calculation is pure attribution, not validation or an
				// authority constructor; malformed changes still need new hashes.
				digest, err := DetachedNoEffectBindingDigest(changed)
				require.NoError(t, err)
				require.NotEqual(t, original.BindingDigest, digest)
			})
		}
	}
	visit(reflect.ValueOf(original), "")
}

// Legal escaped opaque strings expand sixfold. This measures actual canonical
// JSON/base64 widths with every variable scalar at its wire maximum; it is not
// an assertion that a short hand-written record happens to fit 32 KiB.
func TestDetachedNoEffectSharedWireMaxLegalEncoding(t *testing.T) {
	r := detachedNoEffectWireFixture(t)
	q := &r.QuiescenceContext
	q.Namespace = "/" + strings.Repeat("n", 252) + "/" + strings.Repeat("a", 128) + "/" + strings.Repeat("b", 128) + "/"
	require.Len(t, q.Namespace, 512)
	q.AuthorityID, q.Target = strings.Repeat("<", 128), strings.Repeat("<", 128)
	q.RestoreEpoch, q.WorkerID, q.SandboxID = strings.Repeat("r", 128), strings.Repeat("w", 128), strings.Repeat("s", 128)
	q.Runtime = RuntimeReference{ID: strings.Repeat("<", 128), UID: strings.Repeat("<", 128), BootID: strings.Repeat("<", 128)}
	q.Generation, q.DataGateEpoch = math.MaxInt64, math.MaxInt64
	q.ControlRevision, q.ClaimCreateRevision, q.LeaseID = math.MaxInt64-100, math.MaxInt64-90, math.MaxInt64
	r.TaskBirth = q.ControlRevision
	r.EmptyRevision = math.MaxInt64 - 1
	r.WriterClaim.WorkerID, r.FinalSyncClaim.WorkerID = strings.Repeat("w", 128), strings.Repeat("w", 128)
	r.WriterClaim.CreateRevision, r.WriterClaim.LeaseID = math.MaxInt64-30, math.MaxInt64
	r.FinalSyncClaim.CreateRevision, r.FinalSyncClaim.LeaseID = math.MaxInt64-80, math.MaxInt64
	r.Task.CreateRevision, r.Task.ModRevision = r.TaskBirth, r.TaskBirth
	for _, s := range []*RecordStamp{&r.Snapshot, &r.CreationIntent, &r.Publication} {
		s.CreateRevision, s.ModRevision = math.MaxInt64-101, math.MaxInt64-101
	}
	r.Quiescence.CreateRevision, r.Quiescence.ModRevision = math.MaxInt64-60, math.MaxInt64-60
	r.QuiescenceReceipt = r.Quiescence
	r.QuiescenceReservation.CreateRevision, r.QuiescenceReservation.ModRevision = math.MaxInt64-70, math.MaxInt64-70
	r.FinalSync.CreateRevision, r.FinalSync.ModRevision = math.MaxInt64-50, math.MaxInt64-50
	r.Execution.CreateRevision, r.Execution.ModRevision = math.MaxInt64-95, math.MaxInt64
	r.Occupation.CreateRevision, r.Occupation.ModRevision = math.MaxInt64-85, math.MaxInt64-85
	r.QuiescenceStage.RequestID, r.QuiescenceStage.StageID = strings.Repeat("r", 128), strings.Repeat("s", 128)
	r.ExecutionValue = append([]byte(`{"e":"`), bytes.Repeat([]byte{'e'}, 4088)...)
	r.ExecutionValue = append(r.ExecutionValue, []byte(`"}`)...)
	require.Len(t, r.ExecutionValue, 4096)
	hash := sha256.Sum256(r.ExecutionValue)
	r.Execution.SHA256 = hex.EncodeToString(hash[:])
	var err error
	r.BindingDigest, err = DetachedNoEffectBindingDigest(r)
	require.NoError(t, err)
	wire, err := EncodeDetachedNoEffectSettlement(r)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 32768)
	require.Greater(t, len(wire), 8192, "the old generic reservation 8 KiB cap cannot represent this legal input")
	got, err := DecodeDetachedNoEffectSettlement(wire)
	require.NoError(t, err)
	require.Equal(t, r, got)
	t.Logf("detached-noeffect max-legal canonical bytes=%d execution=%d cap=32768", len(wire), len(r.ExecutionValue))
}
