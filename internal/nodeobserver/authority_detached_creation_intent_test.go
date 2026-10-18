package nodeobserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This tests only the actual native envelope/stamp acceptance boundary. Its
// codec fixture is not signed source history or a Task3 writer, and successful
// settlement decoding alone never calls reader.load, a live guard or a kill.
// Published CreationIntent is the original pending key updated in place; its
// create revision must therefore survive alongside the later published mod.
func TestNodeObserverDetachedFactAllowsPublishedCreationIntentRevision(t *testing.T) {
	current := p.TaskCloseDataContext{Namespace: "/observer/mutable-intent/v1/", AuthorityID: "test", Target: "node", RestoreEpoch: "restore",
		IssuerCertificateID: uuid.NewString(), IssuerCertificateDigest: strings.Repeat("a", 64), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("b", 64),
		ClaimID: uuid.NewString(), WorkerID: "old-worker", SandboxID: uuid.NewString(), WorkspaceHash: strings.Repeat("ab", 32), Generation: 1, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 20, LeaseID: 21,
		Runtime: p.RuntimeReference{ID: "plain-pod", UID: uuid.NewString(), BootID: uuid.NewString()},
	}
	creationKey := current.Namespace + "p/ab/intents/" + uuid.NewString()
	creationWire, err := json.Marshal(detachedCreationIntent{Version: 1, IntentID: strings.TrimPrefix(creationKey, current.Namespace+"p/ab/intents/"), SandboxID: current.SandboxID, WorkspaceHash: current.WorkspaceHash, RequestHash: strings.Repeat("c", 64), ConfigurationDigest: strings.Repeat("d", 64), RestoreEpoch: current.RestoreEpoch, Generation: current.Generation, Phase: "published"})
	require.NoError(t, err)
	creation := &mvccpb.KeyValue{Key: []byte(creationKey), Value: creationWire, CreateRevision: 2, ModRevision: 6, Version: 2}
	stamp := func(revision int64) p.RecordStamp {
		return p.RecordStamp{CreateRevision: revision, ModRevision: revision, SHA256: strings.Repeat("b", 64)}
	}
	execution := []byte(`{"version":1}`)
	fact := p.DetachedNoEffectSettlement{Version: 1, Domain: "sandbox.detached-noeffect.v1", Profile: "detached_enrolled_plain", QuiescenceContext: current, Partition: 0xab, TaskBirth: 10,
		CreationIntentID: strings.TrimPrefix(creationKey, current.Namespace+"p/ab/intents/"), Incarnation: uuid.NewSHA1(uuid.NameSpaceOID, []byte("native-envelope-unit")).String(), RunID: uuid.NewString(), Slot: 0, WorkerDigest: strings.Repeat("e", 64),
		WriterClaim: p.DetachedNoEffectClaim{ClaimID: uuid.NewString(), WorkerID: "writer", CreateRevision: 40, LeaseID: 41}, FinalSyncClaim: p.DetachedNoEffectClaim{ClaimID: current.ClaimID, WorkerID: current.WorkerID, CreateRevision: 20, LeaseID: current.LeaseID},
		FinalSyncAttemptID: uuid.NewString(), AttemptID: uuid.NewString(), ContractDigest: strings.Repeat("f", 64), ReceiptDigest: strings.Repeat("a", 64), EmptyRevision: 42,
		Task: stamp(10), Snapshot: stamp(2), CreationIntent: authorityPointStamp(creation), Publication: stamp(6), Quiescence: stamp(25), QuiescenceReceipt: stamp(25), QuiescenceReservation: stamp(23), FinalSync: stamp(30),
		Execution: p.RecordStamp{CreateRevision: 11, ModRevision: 45, SHA256: digest(execution)}, Occupation: stamp(12),
		QuiescenceStage: p.TerminationQuiescenceStage{RequestID: "request", StageID: "stage", AttemptID: uuid.NewString(), Digest: strings.Repeat("c", 64)}, ExecutionValue: execution,
	}
	fact.BindingDigest, err = p.DetachedNoEffectBindingDigest(fact)
	require.NoError(t, err)
	wire, err := p.EncodeDetachedNoEffectSettlement(fact)
	require.NoError(t, err)
	current.CommandID, current.ClaimID, current.WorkerID = uuid.NewString(), uuid.NewString(), "termination-worker"
	current.ClaimCreateRevision, current.LeaseID = 50, 51
	intent := p.TaskRuntimeTerminationIntent{Version: 2, Phase: "reserved", Current: current, Partition: 0xab, TaskBirth: 10, CreationIntentID: fact.CreationIntentID,
		EnrollmentDigest: strings.Repeat("a", 64), ObserverCertificateDigest: strings.Repeat("b", 64), NodeUID: uuid.NewString(), NodeBootID: uuid.NewString(), Action: "kill_pid1_and_observe_cgroup_empty",
		Authority:       p.TerminationAuthorityBindings{Task: fact.Task, CleanupIntent: stamp(10), TaskLink: stamp(10), Placement: stamp(10), Control: stamp(10), Owner: stamp(10), Fence: stamp(10), RuntimeIndex: stamp(10), Quiescence: fact.Quiescence, QuiescenceReceipt: fact.QuiescenceReceipt, QuiescenceReservation: fact.QuiescenceReservation, FinalSync: fact.FinalSync, Issuer: stamp(5), QuiescenceStage: fact.QuiescenceStage},
		DetachedEffects: &p.DetachedRuntimeEffectBindings{Version: 1, Profile: fact.Profile, ContractDigest: fact.ContractDigest, Settlement: p.RecordStamp{CreateRevision: 80, ModRevision: 80, SHA256: digest(wire)}},
	}
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(intent)
	require.NoError(t, err)
	client, _ := detachedMissingFactClient(t, map[string]*mvccpb.KeyValue{
		keys.Settlement: {Key: []byte(keys.Settlement), Value: wire, CreateRevision: 80, ModRevision: 80, Version: 1},
		creationKey:     creation,
	})
	reader := &authorityReader{client: client, binding: p.TrustBinding{Namespace: current.Namespace}, clusterID: 7, timeout: time.Second}
	snapshot := &authoritySnapshot{reader: reader, keys: keys, record: intent, cutoff: time.Now().Add(5 * time.Second)}
	points, err := reader.points(context.Background(), snapshot, []string{creationKey}, []int{4096})
	require.NoError(t, err, "native point reader must retain the actual mutable record envelope")
	require.Equal(t, int64(2), points[0].CreateRevision)
	require.Equal(t, int64(6), points[0].ModRevision)
	require.Equal(t, int64(2), points[0].Version)
	require.NoError(t, reader.loadDetachedSettlement(context.Background(), snapshot), "published CreationIntent stamp is mutable history, independently of the create-once settlement")
	require.NotNil(t, snapshot.detached)
	require.Equal(t, authorityPointStamp(points[0]), snapshot.detached.record.CreationIntent)
}

// The mutable exception belongs only to the fixed CreationIntent source point.
// It still requires a permanent, internally coherent actual MVCC envelope and
// the complete fact-pinned create/mod/hash stamp; it grants no source authority.
func TestNodeObserverDetachedCreationIntentEnvelopeRejectsInvalidNativePoint(t *testing.T) {
	good := &mvccpb.KeyValue{Key: []byte("/observer/test/intents/original"), Value: []byte(`{"phase":"published"}`), CreateRevision: 2, ModRevision: 6, Version: 2}
	stamp := authorityPointStamp(good)
	require.True(t, detachedCreationIntentPointMatches(good, stamp))
	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"leased":                     func(kv *mvccpb.KeyValue) { kv.Lease = 9 },
		"mod before birth":           func(kv *mvccpb.KeyValue) { kv.ModRevision = 1 },
		"zero birth":                 func(kv *mvccpb.KeyValue) { kv.CreateRevision = 0 },
		"zero version":               func(kv *mvccpb.KeyValue) { kv.Version = 0 },
		"negative version":           func(kv *mvccpb.KeyValue) { kv.Version = -1 },
		"version one after mutation": func(kv *mvccpb.KeyValue) { kv.Version = 1 },
		"version exceeds revisions":  func(kv *mvccpb.KeyValue) { kv.Version = 6 },
		"version two at birth":       func(kv *mvccpb.KeyValue) { kv.ModRevision = kv.CreateRevision },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *good
			mutate(&bad)
			// Supply matching revision/hash pins so a stamp mismatch cannot
			// conceal a missing lease, ordering or MVCC-version validation.
			matching := p.RecordStamp{CreateRevision: bad.CreateRevision, ModRevision: bad.ModRevision, SHA256: stamp.SHA256}
			require.False(t, detachedCreationIntentPointMatches(&bad, matching))
		})
	}
	for name, mutate := range map[string]func(*mvccpb.KeyValue){
		"changed create": func(kv *mvccpb.KeyValue) { kv.CreateRevision = 3 },
		"changed mod":    func(kv *mvccpb.KeyValue) { kv.ModRevision = 7 },
		"changed value":  func(kv *mvccpb.KeyValue) { kv.Value = []byte(`{"phase":"pending"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *good
			mutate(&bad)
			require.False(t, detachedCreationIntentPointMatches(&bad, stamp), "a coherent mutable envelope still must retain every original source pin")
		})
	}
	require.False(t, detachedCreationIntentPointMatches(nil, stamp))
	// The ordinary create-once source predicate must remain strict even while
	// the CreationIntent-specific boundary accepts its historical mutation.
	require.False(t, permanentAuthorityPoint(good))
}

// This is only the historical attribution matcher, not a signed source or
// effect capability. The actual HTTP native chain supplies the original Q
// producer, signature verification, runtime drain and observer termination.
func TestNodeObserverDetachedQuiescenceReservationBirth(t *testing.T) {
	for _, name := range []string{"legacy-before", "same-birth", "future", "receipt-birth", "receipt-attempt", "reservation-attempt", "reservation-claim"} {
		t.Run(name, func(t *testing.T) {
			issuer := []byte(`{"certificate":"attribution-only"}`)
			current := p.TaskCloseDataContext{Namespace: "/observer/quiescence/v1/", RestoreEpoch: "restore", TaskID: uuid.NewString(), SandboxID: uuid.NewString(), ClaimID: uuid.NewString(), WorkerID: "original-worker", ClaimCreateRevision: 20, LeaseID: 21, ControlRevision: 10, CommandID: uuid.NewString(), IssuerCertificateID: uuid.NewString(), IssuerCertificateDigest: digest(issuer)}
			ref := authorityTaskReference{current.Namespace, current.RestoreEpoch, current.TaskID, current.SandboxID, 7}
			task := authorityTaskRecord{Version: 1, Reference: ref, Kind: "cleanup", Generation: 1, DataGateEpoch: 1, ControlRevision: 5, ExpiresAt: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)}
			claim := detachedHistoricalClaim{ref, current.ClaimID, current.WorkerID, current.ClaimCreateRevision, current.LeaseID}
			stage := detachedStageReference{current.Namespace, ref.Partition, current.ClaimID, "task_quiesce_users", uuid.NewString(), strings.Repeat("a", 64), current.RestoreEpoch}
			attempt := authorityStage{stage.Namespace, stage.Partition, stage.RequestID, stage.StageID, stage.AttemptID, stage.RestoreEpoch}
			q := detachedQuiescenceRecord{Version: 1, Task: task, Claim: claim, Context: p.TaskUserQuiescenceContext{Current: current, CloseDataIntentRevision: 25}, IssuerCertificateID: current.IssuerCertificateID, IssuerCertificateDigest: digest(issuer), IssuerRevision: 5, Ticket: json.RawMessage(`{"ticket":"attribution-only"}`), CloseDataReceipt: json.RawMessage(`{"receipt":"attribution-only"}`), Attempt: attempt}
			q.TicketDigest = digest(q.Ticket)
			q.Context.CloseDataReceiptDigest = digest(q.CloseDataReceipt)
			reservation := detachedQuiescenceReservation{1, task, claim, 10, current.CommandID, attempt}
			receipt := struct {
				Version uint32 `json:"version"`
				detachedStageReference
				Outcome string `json:"outcome"`
			}{1, stage, "committed"}
			fact := p.DetachedNoEffectSettlement{TaskBirth: 10, QuiescenceContext: current, FinalSyncClaim: p.DetachedNoEffectClaim{ClaimID: claim.ClaimID, WorkerID: claim.WorkerID, CreateRevision: claim.CreateRevision, LeaseID: claim.LeaseID}, ReceiptDigest: strings.Repeat("b", 64), FinalSyncAttemptID: uuid.NewString()}
			final := detachedFinalSyncRecord{Version: 1, Task: task, Claim: claim, DestroyingRevision: 10, QuiescenceRevision: 30, ReceiptDigest: fact.ReceiptDigest, AttemptID: fact.FinalSyncAttemptID}
			final.Quiescence.Task, final.Quiescence.CommandID, final.Quiescence.Stage = ref, current.CommandID, stage
			final.Plan.Mode, final.Plan.Digest = "none", strings.Repeat("c", 64)
			reservationBirth, receiptBirth := int64(30), int64(30)
			switch name {
			case "legacy-before":
				reservationBirth = 29
			case "future":
				reservationBirth = 31
			case "receipt-birth":
				receiptBirth = 29
			case "receipt-attempt":
				receipt.AttemptID = uuid.NewString()
			case "reservation-attempt":
				reservation.Attempt.AttemptID = uuid.NewString()
			case "reservation-claim":
				reservation.Claim.ClaimID = uuid.NewString()
			}
			point := func(value any, birth int64) *mvccpb.KeyValue {
				wire, err := json.Marshal(value)
				require.NoError(t, err)
				return &mvccpb.KeyValue{Value: wire, CreateRevision: birth, ModRevision: birth, Version: 1}
			}
			snapshot := &authoritySnapshot{record: p.TaskRuntimeTerminationIntent{Current: current, Partition: ref.Partition, Authority: p.TerminationAuthorityBindings{QuiescenceStage: p.TerminationQuiescenceStage{RequestID: stage.RequestID, StageID: stage.StageID, AttemptID: stage.AttemptID, Digest: stage.Digest}}}, detached: &detachedAuthoritySnapshot{record: fact}}
			snapshot.history[0], snapshot.history[1], snapshot.history[2] = point(q, 30), point(receipt, receiptBirth), point(reservation, reservationBirth)
			snapshot.history[3], snapshot.history[4] = point(final, 40), point(struct{}{}, 5)
			err := (&authorityReader{}).authenticateDetachedQuiescence(snapshot, task, issuer)
			if name == "legacy-before" || name == "same-birth" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "original quiescence reservation/receipt invalid")
			}
		})
	}
}
