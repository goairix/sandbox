package etcd

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// All inputs are private copied history/configuration data. The builder never
// constructs a live TaskClaim, Prepared effect, kernel permission or provider.
// Its dimensions do not authenticate the input; the caller still loads history
// through the original native readers before binding an execution inventory.
type cleanupLocalWireInput struct {
	bundle      *taskClaimBundle
	state       *cleanupSlotState
	snapshot    SandboxSnapshotRecord
	publication *RuntimeEnrolledPublicationEntry
	issuer      CommandIssuerEntry
	plan        TaskWorkspaceSyncPlan
	workerID    string
	claimTTL    time.Duration
}

// Dimensions describe future wire widths, never an occupied execution or an
// attempt to persist. Early rejection uses fixed full-width UUIDs; the actual
// Grant inventory still supplies and validates its real E/S/Run/Claim binding.
type cleanupLocalWireDimensions struct {
	runID, claimID string
	worker         launcher.WorkspaceSyncWorkerReference
}

// Exactly one concrete protobuf request/response pair represents a Lease RPC.
// Lease operations are not represented by an empty transaction or counted as
// Txn nodes. A native error response never represents successful admission.
type cleanupLeaseWireProjection struct {
	name      string
	grant     *pb.LeaseGrantRequest
	granted   *pb.LeaseGrantResponse
	keepAlive *pb.LeaseKeepAliveRequest
	keptAlive *pb.LeaseKeepAliveResponse
	revoke    *pb.LeaseRevokeRequest
	revoked   *pb.LeaseRevokeResponse
}

type cleanupLocalWireFamily struct {
	name   string
	txns   []cleanupWireProjection
	leases []cleanupLeaseWireProjection
}

type cleanupLocalWirePlan struct {
	families []cleanupLocalWireFamily
}

// These are native shapes, not a count of execution RPCs. Explicit aliases
// below name the same complete request/response at different recovery states.
// Exact subread names prevent a group label from hiding an omitted native read.
var cleanupLocalWireVariants = map[string]string{
	"stage-begin":       "generic-begin stage-grant stage-revoke",
	"stage-commit":      "close-commit checkpoint-commit",
	"stage-resolve":     "close-resolve checkpoint-resolve quiescence-resolve",
	"quiescence-stage":  "reserved-begin reserved-commit",
	"task-checkpoint":   "claim-create claim-post-fence checkpoint-mutation",
	"task-renew":        "renew-before renew-after task-grant task-keepalive",
	"task-release":      "task-revoke",
	"close":             "issuer-read issuer-register close-pre-guard close-committed-guard close-intent-read close-stage-receipt-read",
	"close-query":       "query-close-history query-close-stage-receipt query-close-issuer",
	"quiescence":        "reservation-read reservation-write quiescence-pre-guard quiescence-committed-guard quiescence-intent-read.initial quiescence-intent-read.prior-initial quiescence-intent-read.prior-complete quiescence-receipt-read",
	"quiescence-query":  "query-quiescence-history.initial query-quiescence-history.prior-initial query-quiescence-history.prior-complete query-quiescence-stage-receipt query-quiescence-issuer",
	"dual-drain":        "operations-page empty-before empty-after",
	"final-sync":        "final-sync-read final-sync-reserve",
	"archive-intent":    "archive-pins-before-issuer archive-pins-with-issuer archive-issuer-read archive-issuer-register archive-read archive-write",
	"archive-send":      "archive-guard archive-empty-guard archive-query-guard",
	"archive-recovery":  "intent-admit-read intent-admit-write intent-arbitrate recovery-claim-guard recovery-authority-guard recovery-source-guard.uncharged recovery-source-guard.charged recovery-slot-read recovery-slot-intent-read recovery-reserve recovery-completion-read.intent recovery-completion-read.checkpoint recovery-settle recovery-checkpoint-read recovery-checkpoint-write recovery-recycle",
	"archive-recycling": "candidate-read intent-read intent-arbitrate reserve completion-read.intent completion-read.checkpoint terminal gc",
	"local-progress":    "progress-read progress-initial progress-update progress-charged-update metadata-reserve-read.slot metadata-reserve-read.slot-intent metadata-reserve metadata-guard metadata-terminal metadata-settle",
	"local-complete":    "checkpoint-read completion-read effects-seal completion-publish completion-guard",
	"termination":       "authority-history-read.domain authority-history-read.local-completion authority-history-read.publication-initial authority-history-read.publication-complete authority-history-read.enrollment authority-history-read.archive-final authority-history-read.quiescence-pins termination-read provider-read reserve reserved-guard terminated-write terminated-guard cleanup-complete-write cleanup-complete-guard",
	"owner-release":     "release-proof-read release-history-read.terminal-provider release-history-read.retained-history release",
}

func cleanupLocalFamilyVariants(family string) []string {
	if family != "provider" {
		return strings.Fields(cleanupLocalWireVariants[family])
	}
	names := []string{"provider-initial", "provider-before-termination", "provider-after-termination"}
	for step := 0; step < len(taskRuntimeCleanupSteps); step++ {
		for _, phase := range []string{"intent", "unknown", "complete", "guard"} {
			names = append(names, fmt.Sprintf("step-%d-%s", step, phase))
		}
		if step >= 2 && step <= 5 {
			names = append(names, fmt.Sprintf("step-%d-skipped", step))
		}
	}
	return names
}

func cleanupLocalLeaseKind(family, variant string) string {
	switch family + "/" + variant {
	case "stage-begin/stage-grant", "task-renew/task-grant":
		return "grant"
	case "stage-begin/stage-revoke", "task-release/task-revoke":
		return "revoke"
	case "task-renew/task-keepalive":
		return "keepalive"
	default:
		return ""
	}
}

func validateCleanupLeaseWire(v cleanupLeaseWireProjection) error {
	pairs, requestBytes, responseBytes := 0, 0, 0
	if v.grant != nil || v.granted != nil {
		if v.grant == nil || v.granted == nil || v.grant.TTL <= 0 || v.granted.ID <= 0 || v.granted.TTL <= 0 || v.granted.Header == nil || v.granted.Error != "" {
			return ErrInvalidConfiguration
		}
		pairs++
		requestBytes, responseBytes = v.grant.Size(), v.granted.Size()
	}
	if v.keepAlive != nil || v.keptAlive != nil {
		if v.keepAlive == nil || v.keptAlive == nil || v.keepAlive.ID <= 0 || v.keptAlive.ID != v.keepAlive.ID || v.keptAlive.TTL <= 0 || v.keptAlive.Header == nil {
			return ErrInvalidConfiguration
		}
		pairs++
		requestBytes, responseBytes = v.keepAlive.Size(), v.keptAlive.Size()
	}
	if v.revoke != nil || v.revoked != nil {
		if v.revoke == nil || v.revoked == nil || v.revoke.ID <= 0 || v.revoked.Header == nil {
			return ErrInvalidConfiguration
		}
		pairs++
		requestBytes, responseBytes = v.revoke.Size(), v.revoked.Size()
	}
	if v.name == "" || pairs != 1 || requestBytes > cleanupRequestBytes || responseBytes > maxMutationBytes {
		return ErrInvalidConfiguration
	}
	return nil
}

func validateCleanupLocalWirePlan(plan *cleanupLocalWirePlan) error {
	if plan == nil || len(plan.families) != len(cleanupPhysicalWireFamilies) {
		return ErrInvalidConfiguration
	}
	families := make(map[string]bool, len(plan.families))
	for _, family := range plan.families {
		want := cleanupLocalFamilyVariants(family.name)
		if len(want) == 0 || families[family.name] || len(family.txns)+len(family.leases) != len(want) {
			return ErrInvalidConfiguration
		}
		families[family.name] = true
		seen := make(map[string]bool, len(want))
		for _, projection := range family.txns {
			if seen[projection.name] || cleanupLocalLeaseKind(family.name, projection.name) != "" || projection.request == nil || len(projection.request.Success) == 0 {
				return ErrInvalidConfiguration
			}
			if err := validateCleanupWire(projection); err != nil {
				return err
			}
			seen[projection.name] = true
		}
		for _, lease := range family.leases {
			kind := cleanupLocalLeaseKind(family.name, lease.name)
			if seen[lease.name] || kind == "" || validateCleanupLeaseWire(lease) != nil || kind == "grant" && lease.grant == nil || kind == "keepalive" && lease.keepAlive == nil || kind == "revoke" && lease.revoke == nil {
				return ErrInvalidConfiguration
			}
			seen[lease.name] = true
		}
		for _, name := range want {
			if !seen[name] {
				return fmt.Errorf("%w: cleanup wire profile missing %s/%s", ErrInvalidConfiguration, family.name, name)
			}
		}
	}
	for _, name := range cleanupPhysicalWireFamilies {
		if !families[name] {
			return ErrInvalidConfiguration
		}
	}
	return nil
}

// Existing preflight callers retain their original256KiB checker. Cleanup's
// separate complete plan applies128KiB after these same envelopes are built.
func checkTaskWireProjections(n Namespace, projections []cleanupWireProjection) error {
	for _, projection := range projections {
		if err := taskCheckWire(n, []*pb.TxnRequest{projection.request}, []*pb.TxnResponse{projection.success, projection.failure}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) prepareCleanupLocalWireInventory(input cleanupLocalWireInput, kernel *launcher.KernelBoundary) (*cleanupLocalWireInventory, error) {
	if kernel == nil || input.state == nil || input.state.record.Run == nil {
		return nil, ErrInvalidConfiguration
	}
	worker, err := kernel.WorkspaceSyncWorkerReference()
	if err != nil || worker != input.state.record.Run.Worker {
		return nil, ErrInvalidConfiguration
	}
	plan, err := b.buildCleanupLocalWirePlan(input)
	if err != nil {
		return nil, err
	}
	s := input.state
	inventory := &cleanupLocalWireInventory{origin: b, task: s.record.Task, incarnation: s.record.Incarnation, runID: s.record.Run.ID, claimID: s.record.Pending.ClaimID, executionMod: s.execution.mod, slotMod: s.slot.mod, kernel: kernel, families: plan.families}
	inventory.self = inventory
	return inventory, nil
}

// The assembler retains only owned protobuf and string data. Its fields are
// dimensional inputs; no native API accepts this object as effect authority.
type cleanupLocalWireBuilder struct {
	b          *Backend
	in         cleanupLocalWireInput
	plan       cleanupLocalWirePlan
	claim      TaskClaimReference
	dimensions cleanupLocalWireDimensions

	claimKey, guardKey, claimValue string
	issuerKey, issuerValue         string

	closeKey, closeReceiptKey, quiescenceKey, quiescenceReceiptKey, reservationKey, finalKey, archiveKey string
	intentKey, slotKey, progressKey, completeKey, terminationKey, providerKey, releaseKey, prefix        string

	live, close, quiescence, archive, sync []clientv3.Cmp
	closeStage, quiescenceStage            StageReference
}

func (w *cleanupLocalWireBuilder) add(family, name string, projection cleanupWireProjection) {
	projection.name = name
	for i := range w.plan.families {
		if w.plan.families[i].name == family {
			w.plan.families[i].txns = append(w.plan.families[i].txns, projection)
			return
		}
	}
	w.plan.families = append(w.plan.families, cleanupLocalWireFamily{name: family, txns: []cleanupWireProjection{projection}})
}

func cleanupWireFence(key string, limit int) taskFence {
	return taskFence{key: key, value: string(make([]byte, limit)), create: math.MaxInt64, mod: math.MaxInt64}
}

func cleanupWireAppend(first []clientv3.Cmp, rest ...clientv3.Cmp) []clientv3.Cmp {
	return append(append([]clientv3.Cmp(nil), first...), rest...)
}

func cleanupWirePointProjection(cmps []clientv3.Cmp, keys, values []string) cleanupWireProjection {
	reads, response := taskProtoPoints(keys), taskProtoResponse(keys, values)
	return cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: reads, Failure: reads}, success: response, failure: response}
}

func (w *cleanupLocalWireBuilder) guard(cmps []clientv3.Cmp, emptyFailure bool) cleanupWireProjection {
	keys, values := []string{w.b.identityKey}, []string{w.b.identityValue}
	projection := cleanupWirePointProjection(cmps, keys, values)
	if emptyFailure {
		projection.request.Failure = nil
		projection.failure = taskProtoResponse(nil, nil)
	}
	return projection
}

func (w *cleanupLocalWireBuilder) syncRead(cmps []clientv3.Cmp, keys []string, caps ...int) cleanupWireProjection {
	values := make([]string, len(caps))
	for i, limit := range caps {
		values[i] = string(make([]byte, limit))
	}
	return workspaceSyncNativeProjection(w.b, cmps, taskProtoPoints(keys), taskProtoResponse(keys, values), false)
}

func (b *Backend) buildCleanupLocalWirePlan(input cleanupLocalWireInput) (*cleanupLocalWirePlan, error) {
	if err := b.validateCleanupLocalWireInput(input); err != nil {
		return nil, err
	}
	r := input.state.record
	return b.buildCleanupLocalWireDimensions(input, cleanupLocalWireDimensions{runID: r.Run.ID, claimID: r.Pending.ClaimID, worker: r.Run.Worker})
}

// preflightCleanupLocalWire rejects unsupported metadata/schema/wire profiles
// before E initialization, slot reservation or any new Run. It returns no
// reusable inventory or success token. Grant builds and binds a fresh plan.
func (b *Backend) preflightCleanupLocalWire(input cleanupLocalWireInput, worker launcher.WorkspaceSyncWorkerReference) error {
	_, err := b.buildCleanupLocalWireDimensions(input, cleanupLocalWireDimensions{
		runID:   "11111111-1111-4111-8111-111111111111",
		claimID: "22222222-2222-4222-8222-222222222222", worker: worker,
	})
	return err
}

// All physical request families share this exact assembler. In particular the
// early path keeps whole-record caps and canonical codecs, without fabricating
// an occupied record merely to pass the actual inventory's authority checks.
func (b *Backend) buildCleanupLocalWireDimensions(input cleanupLocalWireInput, dimensions cleanupLocalWireDimensions) (*cleanupLocalWirePlan, error) {
	if err := b.validateCleanupLocalWireMetadata(input); err != nil {
		return nil, err
	}
	if !validPreparationUUID(dimensions.runID) || !validPreparationUUID(dimensions.claimID) || dimensions.worker.Validate() != nil {
		return nil, ErrInvalidRecord
	}
	input.state = nil
	w := &cleanupLocalWireBuilder{b: b, in: input, dimensions: dimensions}
	if err := w.prepare(); err != nil {
		return nil, fmt.Errorf("%w: cleanup wire profile close-commit preparation: %w", ErrInvalidConfiguration, err)
	}
	if err := w.stages(); err != nil {
		return nil, fmt.Errorf("%w: cleanup wire profile stage-commit: %w", ErrInvalidConfiguration, err)
	}
	if err := w.archiveRequests(); err != nil {
		return nil, fmt.Errorf("%w: cleanup wire profile archive-write: %w", ErrInvalidConfiguration, err)
	}
	if err := w.syncRequests(); err != nil {
		return nil, fmt.Errorf("%w: cleanup wire profile metadata-settle: %w", ErrInvalidConfiguration, err)
	}
	if err := w.runtimeRequests(); err != nil {
		return nil, fmt.Errorf("%w: cleanup wire profile terminated-write: %w", ErrInvalidConfiguration, err)
	}
	w.leases()
	if err := validateCleanupLocalWirePlan(&w.plan); err != nil {
		return nil, err
	}
	return &w.plan, nil
}

func (b *Backend) validateCleanupLocalWireInput(in cleanupLocalWireInput) error {
	if err := b.validateCleanupLocalWireMetadata(in); err != nil {
		return err
	}
	state := in.state
	if state == nil {
		return ErrInvalidConfiguration
	}
	if state.record.Task != in.bundle.task.Reference || state.record.Phase != "occupied" || state.record.Run == nil || state.record.Run.Joined || state.record.Pending == nil || state.record.Pending.Purpose != "claim" || state.slot == nil || b.validateCleanupState(state) != nil {
		return ErrInvalidRecord
	}
	return nil
}

func (b *Backend) validateCleanupLocalWireMetadata(in cleanupLocalWireInput) error {
	if b == nil || in.bundle == nil || in.publication == nil || b.enrolledPublication == nil || !validHexDigest(b.enrolledPublication.ContractDigest) || b.workspaceSyncBudget == nil || b.workspaceSyncBudget.profile != 2 || validateTaskWorkspaceSyncBudget(&b.workspaceSyncBudget.options) != nil || in.claimTTL < 30*time.Second || in.claimTTL > 300*time.Second || in.claimTTL%time.Second != 0 || !validDomainSegment(in.workerID) {
		return ErrInvalidConfiguration
	}
	task := in.bundle.task
	if b.validateTaskReference(task.Reference) != nil || task.Validate() != nil || len(in.bundle.fences) != 8 || in.bundle.checkpoint == nil {
		return ErrInvalidRecord
	}
	if in.snapshot.Version != 2 || in.snapshot.LocalCleanup == nil || in.snapshot.SandboxID != task.Reference.SandboxID || in.snapshot.Snapshot != task.Snapshot {
		return ErrInvalidRecord
	}
	if _, err := encodeDomainRecord(in.snapshot); err != nil {
		return err
	}
	workspace, err := in.snapshot.LocalCleanup.workspace()
	if err != nil || workspace.Hash() != task.WorkspaceHash || workspace.Partition() != task.Reference.Partition || workspace.provider != "local" {
		return ErrInvalidRecord
	}
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace, Exclude: in.snapshot.LocalCleanup.Exclude, DeleteExtraneous: in.snapshot.LocalCleanup.DeleteExtraneous})
	if err != nil || plan.digest != in.plan.digest {
		return ErrInvalidRecord
	}
	var placement SandboxPlacementRecord
	if decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(in.bundle.fences[3].value)}, &placement) != nil || validateSnapshotPlacement(&in.snapshot, task.Reference.Partition, task.Reference.SandboxID, task.Snapshot, &placement) != nil {
		return ErrInvalidRecord
	}
	r := in.publication.Record
	if r.Validate() != nil || r.SandboxID != task.Reference.SandboxID || r.WorkspaceHash != task.WorkspaceHash || r.IntentID != task.CreationIntentID || r.Runtime != task.Runtime || r.Snapshot != task.Snapshot || r.Generation != task.Generation || r.DataGateEpoch != task.DataGateEpoch || r.MountAttempt != 0 || in.issuer.Record.Validate() != nil || in.issuer.Record.Namespace != task.Reference.Namespace || in.issuer.Record.RestoreEpoch != task.Reference.RestoreEpoch || in.issuer.Revision <= 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (w *cleanupLocalWireBuilder) leases() {
	header := taskProtoResponse(nil, nil).Header
	for _, family := range cleanupPhysicalWireFamilies {
		for _, name := range cleanupLocalFamilyVariants(family) {
			kind := cleanupLocalLeaseKind(family, name)
			if kind == "" {
				continue
			}
			v := cleanupLeaseWireProjection{name: name}
			switch kind {
			case "grant":
				v.grant = &pb.LeaseGrantRequest{TTL: math.MaxInt64}
				v.granted = &pb.LeaseGrantResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}
			case "keepalive":
				v.keepAlive = &pb.LeaseKeepAliveRequest{ID: math.MaxInt64}
				v.keptAlive = &pb.LeaseKeepAliveResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}
			case "revoke":
				v.revoke = &pb.LeaseRevokeRequest{ID: math.MaxInt64}
				v.revoked = &pb.LeaseRevokeResponse{Header: header}
			}
			found := false
			for i := range w.plan.families {
				if w.plan.families[i].name == family {
					w.plan.families[i].leases = append(w.plan.families[i].leases, v)
					found = true
					break
				}
			}
			if !found {
				w.plan.families = append(w.plan.families, cleanupLocalWireFamily{name: family, leases: []cleanupLeaseWireProjection{v}})
			}
		}
	}
}
