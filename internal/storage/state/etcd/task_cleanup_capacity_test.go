package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A member-stop recovery may retire only the original dispatched Unknown plus
// one of the two exact transport errors observed when killing that member.
// Inspect every leaf: joined corruption, cancellation or unrelated dependency
// failure must not become permission to replace the measured owner.
func metadataScaleOnlyMemberStopTransport(err error) bool {
	unknown, transport := false, false
	var inspect func(error) bool
	inspect = func(err error) bool {
		if err == nil {
			return false
		}
		switch wrapped := err.(type) {
		case interface{ Unwrap() []error }:
			found := false
			for _, child := range wrapped.Unwrap() {
				if child == nil {
					continue
				}
				if !inspect(child) {
					return false
				}
				found = true
			}
			return found
		case interface{ Unwrap() error }:
			return inspect(wrapped.Unwrap())
		default:
			if err == ErrOutcomeUnknown {
				unknown = true
				return true
			}
			if s, ok := status.FromError(err); ok && s.Code() == codes.Unavailable {
				switch s.Message() {
				case "error reading from server: EOF", "error reading from server: connection reset by peer":
					transport = true
					return true
				}
			}
			return false
		}
	}
	return inspect(err) && unknown && transport
}

// This compact test observation is retained from the first failed RPC, never
// reconstructed from logs. A later cleanup failure cannot replace its cause.
// Session fields are locators for the original test owner, not capabilities.
type metadataScaleMaintenanceRPCFailure struct {
	session WorkerSessionReference
	method  string
	defrags [3]metadataScaleDefragInterval

	sent, deadline, returned, sessionDeadline time.Time
	completed                                 bool
	sendContextErr, rpcErr, requestErr        error
	parentBefore, parentAfter                 error
}

// Exactly three actual member calls form this fixture's finite maintenance
// evidence. Epoch is their invocation order; member is the original index.
type metadataScaleDefragInterval struct {
	epoch          uint64
	member         int
	started, ended time.Time
}

// The existing diagnostic mutex must own this update. Cleanup observes the
// same original cause; it cannot turn a later matching timeout into first loss.
func metadataScaleKeepFirstFailure(first *metadataScaleMaintenanceRPCFailure, observed metadataScaleMaintenanceRPCFailure) *metadataScaleMaintenanceRPCFailure {
	if first != nil {
		return first
	}
	return &observed
}

func metadataScaleOnlyDeadlineLeaves(err error, requireUnknown bool) bool {
	leaves, truncated := metadataScaleDiagnosticErrorLeaves(err)
	if truncated || len(leaves) == 0 {
		return false
	}
	unknown, deadline := false, false
	for _, leaf := range leaves {
		if !leaf.Unknown && !leaf.Deadline {
			return false
		}
		unknown = unknown || leaf.Unknown
		deadline = deadline || leaf.Deadline
	}
	return deadline && unknown == requireUnknown
}

// Defrag can block a dispatched worker transaction past its original deadline.
// Permit only the measured loss, not arbitrary timeout recovery: the first RPC
// must start inside an actual Defragment call, then exhaust its deadline and
// return inside that call or one later actual member call. Gaps are excluded.
// The caller still joins the old owner and proves guard/Lease absence before
// acquiring any replacement. Product session ownership remains unchanged.
func metadataScaleObservedDefragDeadline(err error, original WorkerSessionReference, observed *metadataScaleMaintenanceRPCFailure) bool {
	if observed == nil || original.SessionID == "" || original.LeaseID <= 0 || observed.session != original || observed.method != "Txn" || !observed.completed {
		return false
	}
	if observed.sendContextErr != nil || observed.parentBefore != nil || observed.parentAfter != nil || observed.rpcErr != context.DeadlineExceeded || !metadataScaleOnlyDeadlineLeaves(observed.requestErr, false) || !metadataScaleOnlyDeadlineLeaves(err, true) {
		return false
	}
	if observed.sent.IsZero() || !observed.deadline.After(observed.sent) || observed.returned.Before(observed.deadline) || !observed.sessionDeadline.After(observed.returned) {
		return false
	}
	sentInterval, returnedInterval, members := -1, -1, uint8(0)
	for i, interval := range observed.defrags {
		if interval.epoch != uint64(i+1) || interval.member < 0 || interval.member >= 3 || interval.started.IsZero() || !interval.ended.After(interval.started) || i > 0 && observed.defrags[i-1].ended.After(interval.started) {
			return false
		}
		bit := uint8(1) << interval.member
		if members&bit != 0 {
			return false
		}
		members |= bit
		if !observed.sent.Before(interval.started) && observed.sent.Before(interval.ended) {
			sentInterval = i
		}
		if !observed.deadline.Before(interval.started) && !observed.returned.After(interval.ended) {
			returnedInterval = i
		}
	}
	return sentInterval >= 0 && returnedInterval >= sentInterval
}

func TestMetadataScaleObservedDefragDeadline(t *testing.T) {
	now := time.Now()
	original := WorkerSessionReference{SessionID: "original", LeaseID: 1}
	terminal := errors.Join(ErrOutcomeUnknown, context.DeadlineExceeded, context.DeadlineExceeded)
	valid := metadataScaleMaintenanceRPCFailure{
		session: original, method: "Txn",
		sent: now, deadline: now.Add(2 * time.Second), returned: now.Add(2*time.Second + time.Millisecond),
		sessionDeadline: now.Add(time.Minute), completed: true,
		rpcErr: context.DeadlineExceeded, requestErr: context.DeadlineExceeded,
		defrags: [3]metadataScaleDefragInterval{
			{epoch: 1, member: 0, started: now.Add(-time.Second), ended: now.Add(3 * time.Second)},
			{epoch: 2, member: 1, started: now.Add(4 * time.Second), ended: now.Add(7 * time.Second)},
			{epoch: 3, member: 2, started: now.Add(8 * time.Second), ended: now.Add(11 * time.Second)},
		},
	}
	deep := error(context.DeadlineExceeded)
	for range 40 {
		deep = fmt.Errorf("original request: %w", deep)
	}
	many := make([]error, 16)
	for i := range many {
		many[i] = context.DeadlineExceeded
	}
	for _, model := range []struct {
		name   string
		change func(*metadataScaleMaintenanceRPCFailure)
		err    error
		want   bool
	}{
		{name: "actual-defrag-rpc", err: terminal, want: true},
		{name: "cross-ordered-members", err: terminal, want: true, change: func(p *metadataScaleMaintenanceRPCFailure) {
			p.defrags[0].ended = now.Add(time.Second)
			p.defrags[1].started = now.Add(1500 * time.Millisecond)
		}},
		{name: "mixed-corrupt", err: errors.Join(terminal, ErrCorruptRecord)},
		{name: "mixed-canceled", err: errors.Join(terminal, context.Canceled)},
		{name: "missing-unknown", err: context.DeadlineExceeded},
		{name: "missing-terminal", err: nil},
		{name: "deep-terminal", err: errors.Join(ErrOutcomeUnknown, deep)},
		{name: "truncated-terminal", err: errors.Join(ErrOutcomeUnknown, errors.Join(many...))},
		{name: "outside-defrag", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[0].started = now.Add(time.Millisecond) }},
		{name: "foreign-epoch", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[1].epoch = 4 }},
		{name: "foreign-member", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[1].member = 3 }},
		{name: "duplicate-member", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[1].member = 0 }},
		{name: "backward-intervals", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) {
			p.defrags[1].started, p.defrags[2].started = p.defrags[2].started, p.defrags[1].started
			p.defrags[1].ended, p.defrags[2].ended = p.defrags[2].ended, p.defrags[1].ended
		}},
		{name: "overlapping-intervals", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[1].started = now }},
		{name: "deadline-in-gap", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) {
			p.defrags[0].ended = now.Add(time.Second)
			p.defrags[1].started = p.deadline.Add(time.Microsecond)
		}},
		{name: "parent-expired", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.parentAfter = context.DeadlineExceeded }},
		{name: "parent-canceled-before-send", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.parentBefore = context.Canceled }},
		{name: "canceled-before-send", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.sendContextErr = context.Canceled }},
		{name: "locally-expired-before-call", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.sendContextErr = context.DeadlineExceeded }},
		{name: "no-original-session", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.session = WorkerSessionReference{} }},
		{name: "foreign-session", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.session.SessionID = "foreign" }},
		{name: "foreign-lease", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.session.LeaseID++ }},
		{name: "incomplete-rpc", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.completed = false }},
		{name: "invalid-deadline", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.deadline = p.sent }},
		{name: "deadline-passed-at-call", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.sent = p.deadline.Add(time.Nanosecond) }},
		{name: "session-expired-first", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.sessionDeadline = p.deadline }},
		{name: "session-expired-at-return", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.sessionDeadline = p.returned }},
		{name: "returned-before-deadline", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.returned = p.sent }},
		{name: "returned-after-defrag", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[0].ended = p.returned.Add(-time.Microsecond) }},
		{name: "no-completed-defrag", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.defrags[0].ended = time.Time{} }},
		{name: "canceled-rpc", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.rpcErr = context.Canceled }},
		{name: "mixed-request", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) {
			p.requestErr = errors.Join(context.DeadlineExceeded, ErrCorruptRecord)
		}},
		{name: "missing-request", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.requestErr = nil }},
		{name: "deep-request", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.requestErr = deep }},
		{name: "truncated-request", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.requestErr = errors.Join(many...) }},
		{name: "cleanup-rpc", err: terminal, change: func(p *metadataScaleMaintenanceRPCFailure) { p.method = "Revoke" }},
	} {
		t.Run(model.name, func(t *testing.T) {
			proof := valid
			if model.change != nil {
				model.change(&proof)
			}
			require.Equal(t, model.want, metadataScaleObservedDefragDeadline(model.err, original, &proof))
		})
	}
	require.False(t, metadataScaleObservedDefragDeadline(terminal, original, nil))

	// Actual completion, not delayed observer publication, bounds Defrag.
	// The RPC returned between those two moments and therefore must fail.
	late := valid
	late.defrags[0].ended = late.returned.Add(-time.Millisecond)
	published := late.returned.Add(time.Millisecond)
	require.True(t, late.defrags[0].ended.Before(late.returned))
	require.True(t, late.returned.Before(published))
	require.False(t, metadataScaleObservedDefragDeadline(terminal, original, &late))

	outside := valid
	outside.defrags[0].started = outside.sent.Add(time.Millisecond)
	first := metadataScaleKeepFirstFailure(nil, outside)
	require.Same(t, first, metadataScaleKeepFirstFailure(first, valid))
	require.False(t, metadataScaleObservedDefragDeadline(terminal, original, first), "a later matching failure must not replace the first outside-phase cause")
	first = metadataScaleKeepFirstFailure(nil, valid)
	cleanup := valid
	cleanup.method = "Revoke"
	require.Same(t, first, metadataScaleKeepFirstFailure(first, cleanup))
	require.True(t, metadataScaleObservedDefragDeadline(terminal, original, first))
}

// Diagnostic leaves preserve error identity and gRPC status. They are not
// sufficient recovery evidence: the checker above also requires the original
// owner and exact Defrag/RPC lifetime. Truncated trees always fail that check.
// The small tree/leaf/text limits also bound malformed or cyclic test errors.
type metadataScaleErrorLeaf struct {
	Type, Code, Message         string
	Unknown, Deadline, Canceled bool
}

func metadataScaleDiagnosticErrorLeaves(err error) (leaves []metadataScaleErrorLeaf, truncated bool) {
	nodes := 0
	var visit func(error)
	visit = func(current error) {
		if current == nil || truncated {
			return
		}
		nodes++
		if nodes > 32 || len(leaves) >= 8 {
			truncated = true
			return
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range wrapped.Unwrap() {
				visit(child)
				if truncated {
					return
				}
			}
		case interface{ Unwrap() error }:
			visit(wrapped.Unwrap())
		default:
			message := current.Error()
			if len(message) > 256 {
				message = message[:256] + "[truncated]"
			}
			leaf := metadataScaleErrorLeaf{
				Type: fmt.Sprintf("%T", current), Message: message,
				Unknown:  current == ErrOutcomeUnknown,
				Deadline: current == context.DeadlineExceeded,
				Canceled: current == context.Canceled,
			}
			if grpc, ok := status.FromError(current); ok {
				leaf.Code = grpc.Code().String()
			}
			leaves = append(leaves, leaf)
		}
	}
	visit(err)
	return leaves, truncated
}

func TestMetadataScaleDiagnosticErrorLeaves(t *testing.T) {
	joined := errors.Join(ErrOutcomeUnknown, context.DeadlineExceeded, context.DeadlineExceeded)
	leaves, truncated := metadataScaleDiagnosticErrorLeaves(joined)
	require.False(t, truncated)
	require.Len(t, leaves, 3)
	require.True(t, leaves[0].Unknown)
	require.True(t, leaves[1].Deadline)
	require.True(t, leaves[2].Deadline)
	require.NotEmpty(t, leaves[1].Type)
	require.False(t, metadataScaleOnlyMemberStopTransport(joined), "diagnosis must not broaden the original recovery permission")

	mixed := errors.Join(ErrOutcomeUnknown, status.Error(codes.Unavailable, "error reading from server: EOF"), context.Canceled, ErrCorruptRecord)
	leaves, truncated = metadataScaleDiagnosticErrorLeaves(mixed)
	require.False(t, truncated)
	require.Len(t, leaves, 4)
	require.Equal(t, codes.Unavailable.String(), leaves[1].Code)
	require.True(t, leaves[2].Canceled)
	require.False(t, metadataScaleOnlyMemberStopTransport(mixed))

	large := make([]error, 16)
	for i := range large {
		large[i] = errors.New(strings.Repeat("x", 512))
	}
	leaves, truncated = metadataScaleDiagnosticErrorLeaves(errors.Join(large...))
	require.True(t, truncated)
	require.Len(t, leaves, 8)
	for _, leaf := range leaves {
		require.LessOrEqual(t, len(leaf.Message), 267)
	}
}

func TestMetadataScaleMemberStopErrorClassification(t *testing.T) {
	eof := status.Error(codes.Unavailable, "error reading from server: EOF")
	reset := status.Error(codes.Unavailable, "error reading from server: connection reset by peer")
	for _, model := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "actual-pair", err: errors.Join(ErrOutcomeUnknown, eof), want: true},
		{name: "wrapped-pair", err: fmt.Errorf("original guard: %w", errors.Join(ErrOutcomeUnknown, eof)), want: true},
		{name: "actual-reset-pair", err: errors.Join(ErrOutcomeUnknown, reset), want: true},
		{name: "nil"},
		{name: "only-unknown", err: ErrOutcomeUnknown},
		{name: "only-eof", err: eof},
		{name: "corruption", err: errors.Join(ErrOutcomeUnknown, eof, ErrCorruptRecord)},
		{name: "reset-corruption", err: errors.Join(ErrOutcomeUnknown, reset, ErrCorruptRecord)},
		{name: "canceled", err: errors.Join(ErrOutcomeUnknown, eof, context.Canceled)},
		{name: "other-unavailable", err: errors.Join(ErrOutcomeUnknown, status.Error(codes.Unavailable, "other dependency failure"))},
	} {
		t.Run(model.name, func(t *testing.T) {
			require.Equal(t, model.want, metadataScaleOnlyMemberStopTransport(model.err))
		})
	}
}

// Startup diagnosis retains both cardinalities and their shared checkpoint
// history, but cannot select either full idle acceptance or the fault pilot.
func metadataScaleRunPlan(pilot, faultPilot, startupPilot bool) (time.Duration, []int, error) {
	if faultPilot && !pilot || startupPilot && (pilot || faultPilot) {
		return 0, nil, fmt.Errorf("metadata scale pilot modes must be selected independently")
	}
	if startupPilot {
		return 0, []int{10000, 100000}, nil
	}
	if pilot {
		return time.Minute, []int{10000}, nil
	}
	return time.Hour, []int{10000, 100000}, nil
}

func TestMetadataScaleRunPlanPreservesFullAndStartupBoundaries(t *testing.T) {
	for _, model := range []struct {
		name                       string
		pilot, faultPilot, startup bool
		duration                   time.Duration
		tiers                      []int
		invalid                    bool
	}{
		{name: "full", duration: time.Hour, tiers: []int{10000, 100000}},
		{name: "pilot", pilot: true, duration: time.Minute, tiers: []int{10000}},
		{name: "fault-pilot", pilot: true, faultPilot: true, duration: time.Minute, tiers: []int{10000}},
		{name: "startup-only", startup: true, tiers: []int{10000, 100000}},
		{name: "fault-without-pilot", faultPilot: true, invalid: true},
		{name: "startup-and-pilot", pilot: true, startup: true, invalid: true},
		{name: "startup-and-fault", pilot: true, faultPilot: true, startup: true, invalid: true},
	} {
		t.Run(model.name, func(t *testing.T) {
			duration, tiers, err := metadataScaleRunPlan(model.pilot, model.faultPilot, model.startup)
			if model.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, model.duration, duration)
			require.Equal(t, model.tiers, tiers)
		})
	}
}

// metadataScaleModel creates the typed metadata subset needed by View/Snapshot
// and due scheduling: control, snapshot, owner, fence, placement, runtime index,
// creation intent, creation request and due candidate. It omits signed runtime
// publication/dispatch, quota and terminal histories. Its encoded footprint is
// B_model_subset, not a complete B_live or an estimate of usable cell capacity.
// Operator seeding grants no effects and represents no actual Pods or mounts.
func metadataScaleModel(n Namespace, ordinal int, expiry time.Time) ([]Write, string, error) {
	if ordinal < 0 || ordinal >= 100000 || !validDomainExpiry(expiry) {
		return nil, "", ErrInvalidRecord
	}
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("metadata-scale-sandbox/%d", ordinal))).String()
	intent := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("metadata-scale-intent/%d", ordinal))).String()
	w, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("metadata-scale/%08d/", ordinal))
	if err != nil {
		return nil, "", err
	}
	payload := json.RawMessage(`{"model":"metadata-only","body":"` + strings.Repeat("x", []int{2048, 6144, 10240}[ordinal%3]) + `"}`)
	digest, err := snapshotDigest(payload)
	if err != nil {
		return nil, "", err
	}
	runtime := RuntimeReference{ID: id, UID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("runtime/"+id)).String(), BootID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("boot/"+id)).String()}
	snapshot := SnapshotReference{Version: "model-v1", Digest: digest}
	control := SandboxControlRecord{Version: 1, SandboxID: id, WorkspaceHash: w.Hash(), IntentID: intent, RestoreEpoch: "test-epoch", Generation: 1, DataGateEpoch: 1, Phase: PhaseActive, Snapshot: snapshot, Runtime: &runtime, ExpiresAt: expiry}
	requestHash := workerIdentityDigest("metadata-scale-request/" + id)
	var writes []Write
	add := func(parts []string, record interface{ Validate() error }) {
		if err != nil {
			return
		}
		var key, value string
		key, err = n.Key(parts...)
		if err == nil {
			value, err = encodeDomainRecord(record)
		}
		if err == nil {
			writes = append(writes, Write{Key: key, Value: []byte(value)})
		}
	}
	prefix := []string{"p", fmt.Sprintf("%02x", w.Partition())}
	add(append(append([]string(nil), prefix...), "controls", id), control)
	add(append(append([]string(nil), prefix...), "snapshots", id, snapshot.Version), SandboxSnapshotRecord{Version: 1, SandboxID: id, Snapshot: snapshot, Payload: payload})
	add(append(append([]string(nil), prefix...), "workspaces", w.Hash(), "owner"), WorkspaceOwnerRecord{1, w.Hash(), id, intent, control.RestoreEpoch, 1, &runtime, 0})
	add(append(append([]string(nil), prefix...), "workspaces", w.Hash(), "fence"), WorkspaceFenceRecord{1, w.Hash(), control.RestoreEpoch, 1})
	add([]string{"indexes", "sandbox", id}, SandboxPlacementRecord{1, id, w.Hash(), intent, control.RestoreEpoch, w.Partition(), 1})
	indexKey, indexErr := n.runtimeIndexKey(runtime.UID)
	if indexErr != nil {
		return nil, "", indexErr
	}
	index := RuntimeIndexRecord{1, intent, id, w.Hash(), control.RestoreEpoch, 1, runtime}
	value, indexErr := encodeDomainRecord(index)
	if indexErr != nil {
		return nil, "", indexErr
	}
	writes = append(writes, Write{Key: indexKey, Value: []byte(value)})
	add(append(append([]string(nil), prefix...), "intents", intent), CreationIntentRecord{1, intent, id, w.Hash(), requestHash, digest, control.RestoreEpoch, 1, "published"})
	add([]string{"requests", requestHash}, CreationRequestRecord{1, "model-request", requestHash, digest, intent, id, w.Hash(), control.RestoreEpoch, 1, "completed"})
	due := dueCandidateForControl(w.Partition(), control)
	dueKey, dueErr := n.dueCandidateKey(due)
	if dueErr != nil {
		return nil, "", dueErr
	}
	value, dueErr = encodeDueCandidate(due)
	if dueErr != nil {
		return nil, "", dueErr
	}
	writes = append(writes, Write{Key: dueKey, Value: []byte(value)})
	return writes, id, err
}

// At most three complete nine-key models share one atomic seed transaction.
// Both actual native branches and the identity/restore comparisons count:
// base4 + absent27 + Put27 + Else2 =60 nodes. Byte limits use actual protobuf.
func metadataScaleSeedRequest(b *Backend, writes []Write) (*pb.TxnRequest, error) {
	if b == nil || len(writes) == 0 || len(writes) > 27 || len(writes)%9 != 0 {
		return nil, ErrInvalidRecord
	}
	bytes := 0
	cmps := b.baseComparisons()
	for i, write := range writes {
		if !validNamespaceKey(b.namespace, write.Key) || write.Delete || len(write.Value) > 65536 || bytes > (100<<10)-len(write.Value) {
			return nil, ErrInvalidRecord
		}
		for _, prior := range writes[:i] {
			if prior.Key == write.Key {
				return nil, ErrInvalidRecord
			}
		}
		bytes += len(write.Value)
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(write.Key), "=", 0))
	}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes), Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey})}
	if len(request.Compare)+len(request.Success)+len(request.Failure) > 64 || request.Size() > 128<<10 {
		return nil, ErrInvalidRecord
	}
	return request, nil
}

func TestMetadataScaleModelCanonicalAndBatchBounds(t *testing.T) {
	n, err := NewNamespace("/scale-test", "metadata", "v1")
	require.NoError(t, err)
	b := &Backend{namespace: n, identityValue: `{"schema_version":1}`, restoreEpoch: "test-epoch"}
	b.identityKey, err = n.Key("meta", "identity")
	require.NoError(t, err)
	b.restoreKey, err = n.Key("meta", "restore_epoch")
	require.NoError(t, err)
	var batch []Write
	for _, ordinal := range []int{0, 1, 99998} {
		writes, id, err := metadataScaleModel(n, ordinal, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.Len(t, writes, 9)
		var control SandboxControlRecord
		var snapshot SandboxSnapshotRecord
		require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: writes[0].Value}, &control))
		require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: writes[1].Value}, &snapshot))
		require.Equal(t, id, control.SandboxID)
		require.Equal(t, control.Snapshot, snapshot.Snapshot)
		require.Equal(t, "test-epoch", control.RestoreEpoch)
		batch = append(batch, writes...)
	}
	request, err := metadataScaleSeedRequest(b, batch)
	require.NoError(t, err)
	require.Equal(t, 60, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.LessOrEqual(t, request.Size(), 128<<10)
	_, err = metadataScaleSeedRequest(b, append(batch, batch[:9]...))
	require.Error(t, err)
	bad := append([]Write(nil), batch...)
	bad[0].Value = make([]byte, 65537)
	_, err = metadataScaleSeedRequest(b, bad)
	require.Error(t, err)
}

// A peer owns one real session and partition. Its sole test goroutine rotates
// over 25 disjoint original Claim shares; readiness never changes a bucket.
// These are metadata reservation calls, not physical cleanup authority.
type cleanupCapacityPeer struct {
	b       *Backend
	session *WorkerSession
	part    *WorkerPartitionClaim
	shares  []*WorkerBudgetShare
	next    int
	lease   *creationFaultLease

	rpcs      atomic.Int64
	claimPuts atomic.Int64
	dropped   atomic.Int64
	dropRev   atomic.Int64
	turns     int
	conflicts int
	unknown   *cleanupSlotState
	lastOwner *workerDueOwner
}

type cleanupCapacityTaskState struct {
	ref      TaskReference
	evidence *cleanupSlotEvidence
	selected *cleanupSlotState
	occupied *cleanupSlotState
}

// Only actual successful upstream reserve responses can be withheld. All
// requests still reach etcd; there is no synthetic success or authority hook.
func (p *cleanupCapacityPeer) observeNative(dropFirstReserve bool) {
	reserve := false
	p.b.client.KV = &faultKV{KV: p.b.client.KV, match: func(ops []clientv3.Op) bool {
		p.rpcs.Add(1)
		reserve = false
		for _, op := range ops {
			if !op.IsPut() {
				continue
			}
			key := string(op.KeyBytes())
			if strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/claim") {
				p.claimPuts.Add(1)
			}
			if strings.HasPrefix(key, p.b.namespace.Root()+"cleanup-execution-slots/") {
				reserve = true
			}
		}
		return true
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if dropFirstReserve && reserve && err == nil && r != nil && r.Succeeded && p.dropped.CompareAndSwap(0, 1) {
			p.dropRev.Store(r.Header.Revision)
			return nil, errors.New("capacity fixture: actual committed reserve ACK withheld")
		}
		return r, err
	}}
	p.lease = &creationFaultLease{Lease: p.b.client.Lease}
	p.b.client.Lease = p.lease
}

// Each turn earns and consumes one original credit, retains the native owner
// through RPC return/postcheck, then joins it before any later turn or renewal.
// The callback is test orchestration of existing private protocol methods only.
func (p *cleanupCapacityPeer) turn(ctx context.Context, work func(*workerDueOwner) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.b.archiveLife.mu.Lock()
		remaining := time.Until(p.session.deadline)
		p.b.archiveLife.mu.Unlock()
		if remaining < 15*time.Second {
			if err := p.b.RenewWorkerSession(ctx, p.session); err != nil {
				return err
			}
		}

		now := time.Now()
		var ready *WorkerBudgetShare
		var earliest time.Time
		p.b.archiveLife.mu.Lock()
		for n := 0; n < len(p.shares); n++ {
			i := (p.next + n) % len(p.shares)
			at := dueWorkerCreditAt(p.shares[i].bucket, now, 1)
			if !at.After(now) {
				ready = p.shares[i]
				p.next = (i + 1) % len(p.shares)
				break
			}
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
		p.b.archiveLife.mu.Unlock()
		if ready == nil {
			timer := time.NewTimer(time.Until(earliest))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}

		owner, stop, err := p.b.beginExpiredTaskOwner(ctx, p.part, ready)
		if err != nil {
			return err
		}
		p.turns++
		p.lastOwner = owner
		before := p.rpcs.Load()
		err = work(owner)
		post := owner.postcheck()
		owner.finish()
		stop()
		<-owner.done
		if count := p.rpcs.Load() - before; count > 2 {
			return fmt.Errorf("charged turn sent %d RPCs, maximum 2", count)
		}
		return errors.Join(err, post)
	}
}

func (p *cleanupCapacityPeer) prepare(ctx context.Context, task *cleanupCapacityTaskState) error {
	var bundle *taskClaimBundle
	if err := p.turn(ctx, func(o *workerDueOwner) error {
		var err error
		bundle, err = p.b.loadCleanupClaimBundle(o, task.ref)
		return err
	}); err != nil {
		return err
	}
	if err := p.turn(ctx, func(o *workerDueOwner) error {
		var err error
		task.evidence, err = p.b.loadCleanupSlotEvidence(o, bundle)
		return err
	}); err != nil {
		return err
	}
	return p.turn(ctx, func(o *workerDueOwner) error {
		if err := p.b.initializeCleanupSlot(o, task.evidence); err != nil {
			return err
		}
		var err error
		task.selected, err = p.b.readCleanupSlot(o, task.ref)
		return err
	})
}

// Every conflict yields its native owner and consumes another real credit for
// rotation. An uncertain successful reserve is read at its exact candidate;
// it never enters the conflict/rotation branch or creates another attempt.
func (p *cleanupCapacityPeer) occupy(ctx context.Context, task *cleanupCapacityTaskState, expectFull bool) error {
	seen := make(map[uint32]bool)
	for candidate := 0; candidate < 32; candidate++ {
		selected := task.selected
		if seen[selected.record.Slot] {
			return fmt.Errorf("candidate repeated before full 32-slot traversal")
		}
		seen[selected.record.Slot] = true
		err := p.turn(ctx, func(o *workerDueOwner) error {
			return p.b.reserveCleanupSlot(o, task.evidence, selected)
		})
		if err == nil || errors.Is(err, ErrOutcomeUnknown) {
			unknown := errors.Is(err, ErrOutcomeUnknown)
			if err = p.turn(ctx, func(o *workerDueOwner) error {
				var readErr error
				task.occupied, readErr = p.b.readCleanupKnownState(o, selected)
				return readErr
			}); err != nil {
				return err
			}
			if task.occupied.record.Selection != selected.record.Selection || task.occupied.record.Incarnation != selected.record.Incarnation {
				return fmt.Errorf("reserve changed candidate or incarnation")
			}
			if unknown {
				if task.occupied.execution.mod != p.dropRev.Load() {
					return fmt.Errorf("unknown resolution does not match actual commit revision")
				}
				p.unknown = task.occupied
			}
			if expectFull {
				return fmt.Errorf("33rd Task unexpectedly occupied slot %d", selected.record.Slot)
			}
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		p.conflicts++
		if candidate == 31 {
			break
		}
		if err = p.turn(ctx, func(o *workerDueOwner) error {
			if err := p.b.rotateCleanupSlot(o, selected); err != nil {
				return err
			}
			var readErr error
			task.selected, readErr = p.b.readCleanupSlot(o, task.ref)
			return readErr
		}); err != nil {
			return err
		}
	}
	if !expectFull {
		return fmt.Errorf("fewer than 32 Tasks exhausted all slots")
	}
	return nil
}

// This producer uses original domain APIs and genuine signed ordinary runtime
// metadata. It creates no Pod and supplies no Kernel/physical permission.
func cleanupCapacityTask(t *testing.T, ctx context.Context, b *Backend, root ed25519.PrivateKey, workspace WorkspaceIdentity, ordinal int) TaskReference {
	t.Helper()
	in := acquisitionInput(t, fmt.Sprintf("capacity-%d", ordinal))
	in.Workspace = workspace
	acquired, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	claim, err := b.ClaimCreation(ctx, workspace, in.IntentID, "capacity-producer", 30*time.Second)
	require.NoError(t, err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ReleaseCreationClaim(cleanup, claim))
	}()
	dispatch, err := b.DeclareRuntimeDispatch(ctx, claim, dispatchInputForDeclaration())
	require.NoError(t, err)
	certificate := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", RuntimeReference{
		ID: fmt.Sprintf("capacity-runtime-%d", ordinal), UID: fmt.Sprintf("capacity-uid-%d", ordinal), BootID: fmt.Sprintf("capacity-boot-%d", ordinal),
	})
	bound, err := b.BindRuntime(ctx, claim, certificate)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mounted, err := b.ConsumeRuntimeMount(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	preparation, err := b.loadRuntimePreparation(ctx, workspace, in.IntentID, "")
	require.NoError(t, err)
	now := time.Now().UTC()
	proof, err := controlprotocol.SignReadyReceipt(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)), certificate, controlprotocol.ReadyReceiptClaims{
		Version: 1, CertificateDigest: preparation.Binding.Record.CertificateDigest,
		Claim:         controlprotocol.ClaimReference{ClaimID: claim.reference.ClaimID, CreateRevision: claim.reference.CreateRevision, LeaseID: claim.reference.LeaseID},
		DataGateEpoch: claim.control.DataGateEpoch, GateState: "open", MountAttempt: preparation.Mount.Record.MountAttempt,
		ObservedAt: now, ValidUntil: now.Add(5 * time.Second),
	})
	require.NoError(t, err)
	published, err := b.PublishRuntime(ctx, claim, proof)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	control, err := b.LoadControl(ctx, workspace.Partition(), in.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := b.namespace.sandboxKeys(workspace.Partition(), in.SandboxID, control.Snapshot.Version)
	require.NoError(t, err)
	points, err := b.readDomain(ctx, controlKey)
	require.NoError(t, err)
	require.NotNil(t, points[0])
	stage, ref, err := b.PrepareDestroy(ctx, BeginDestroyInput{SandboxID: in.SandboxID, RequestID: fmt.Sprintf("capacity-destroy-%d", ordinal), ExpectedControlRevision: points[0].ModRevision}, 30*time.Second)
	require.NoError(t, err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ReleaseStage(cleanup, stage))
	}()
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	return ref
}

// Observation reads are test evidence, not admission or discovery authority.
// Preserve every E/S byte and native revision for the full/lease-loss checks.
func cleanupCapacityRows(t *testing.T, ctx context.Context, raw *clientv3.Client, root string) map[string]*mvccpb.KeyValue {
	t.Helper()
	response, err := raw.Get(ctx, root, clientv3.WithPrefix())
	require.NoError(t, err)
	rows := make(map[string]*mvccpb.KeyValue)
	for _, kv := range response.Kvs {
		key := string(kv.Key)
		if strings.HasPrefix(key, root+"cleanup-execution-slots/") || strings.Contains(key, "/tasks/") && strings.HasSuffix(key, "/execution") {
			rows[key] = kv
		}
	}
	return rows
}

func TestCleanupPersistent32RealTasks(t *testing.T) {
	require.NotEmpty(t, os.Getenv("TEST_ETCD_ENDPOINTS"), "real three-member fixture is mandatory; missing configuration is not a skipped PASS")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	base, raw := integrationBackend(t) // Exactly one cell for every producer and worker.
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options := integrationOptions(base, raw)
	options.RequestTimeout = time.Second
	options.Clock = goodAuthorityClock()
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	options.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
	profileKey, err := base.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	_, err = raw.Put(ctx, profileKey, workerTestProfile(options))
	require.NoError(t, err)
	producer, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, producer.Close()) })

	var groups [4][]*cleanupCapacityTaskState
	var extra *cleanupCapacityTaskState
	// This bounded pure search only chooses routing; every persisted Task is
	// subsequently produced by the original atomic lifecycle above.
	for candidate := 0; candidate < 65536; candidate++ {
		workspace, err := NewWorkspaceIdentity("s3", "test-storage", "bucket", fmt.Sprintf("capacity/%d/", candidate))
		require.NoError(t, err)
		partition := int(workspace.Partition())
		if partition >= 4 || len(groups[partition]) >= 8 && (partition != 0 || extra != nil) {
			continue
		}
		ordinal := partition*8 + len(groups[partition])
		if len(groups[partition]) == 8 {
			ordinal = 32
		}
		task := &cleanupCapacityTaskState{ref: cleanupCapacityTask(t, ctx, producer, root, workspace, ordinal)}
		if ordinal == 32 {
			extra = task
		} else {
			groups[partition] = append(groups[partition], task)
		}
		if extra != nil && len(groups[0])+len(groups[1])+len(groups[2])+len(groups[3]) == 32 {
			break
		}
	}
	require.NotNil(t, extra)
	var peers [4]*cleanupCapacityPeer
	for i := range peers {
		require.Len(t, groups[i], 8)
		b, err := New(ctx, options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, b.Close()) })
		session, err := b.OpenWorkerSession(ctx, fmt.Sprintf("capacity-worker-%d", i))
		require.NoError(t, err)
		part, err := b.ClaimWorkerPartition(ctx, session, uint8(i))
		require.NoError(t, err)
		peer := &cleanupCapacityPeer{b: b, session: session, part: part}
		peers[i] = peer
		for slot := i * 25; slot < (i+1)*25; slot++ {
			share, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, uint16(slot))
			require.NoError(t, err)
			peer.shares = append(peer.shares, share)
		}
		peer.observeNative(i == 0)
	}

	ready, start := make(chan struct{}, 4), make(chan struct{})
	results := make(chan error, 4)
	var workers sync.WaitGroup
	var release sync.Once
	unblock := func() { release.Do(func() { close(start) }) }
	// Register before launching. Fatal/timeout must cancel, unblock and join all
	// four native owners before any Backend cleanup can acquire their lifetimes.
	defer func() {
		cancel()
		unblock()
		workers.Wait()
	}()
	for i, peer := range peers {
		workers.Add(1)
		go func(p *cleanupCapacityPeer, tasks []*cleanupCapacityTaskState) {
			defer workers.Done()
			var result error
			defer func() { results <- result }()
			for _, task := range tasks {
				if result = p.prepare(ctx, task); result != nil {
					return
				}
			}
			ready <- struct{}{}
			select {
			case <-ctx.Done():
				result = ctx.Err()
				return
			case <-start:
			}
			for _, task := range tasks {
				if result = p.occupy(ctx, task, false); result != nil {
					return
				}
			}
		}(peer, groups[i])
	}
	for i := 0; i < 4; i++ {
		select {
		case <-ready:
		case err := <-results:
			t.Fatalf("prepare barrier failed: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	unblock()
	for i := 0; i < 4; i++ {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	workers.Wait()

	occupied := make(map[uint32]TaskReference)
	for _, tasks := range groups {
		for _, task := range tasks {
			s := task.occupied
			require.NotNil(t, s)
			require.Equal(t, "occupied", s.record.Phase)
			require.Equal(t, task.ref, s.record.Task)
			require.Equal(t, cleanupSlotTestIncarnation(s.record), s.record.Incarnation)
			require.Equal(t, task.evidence.bundle.birth, s.record.TaskBirth)
			require.NotNil(t, s.slot)
			require.Zero(t, s.slot.lease)
			require.Zero(t, s.execution.lease)
			require.Greater(t, s.slot.create, s.execution.create)
			require.Equal(t, s.slot.mod, s.execution.mod)
			_, duplicate := occupied[s.record.Slot]
			require.False(t, duplicate)
			occupied[s.record.Slot] = task.ref
		}
	}
	require.Len(t, occupied, 32)
	require.Equal(t, int64(1), peers[0].dropped.Load())
	require.NotNil(t, peers[0].unknown)
	full := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Len(t, full, 64)

	p := peers[0]
	require.NoError(t, p.prepare(ctx, extra))
	beforeConflicts := p.conflicts
	require.NoError(t, p.occupy(ctx, extra, true))
	require.Equal(t, 32, p.conflicts-beforeConflicts)
	require.Nil(t, extra.occupied)
	require.Equal(t, "selecting", extra.selected.record.Phase)
	afterFull := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Len(t, afterFull, 65)
	require.True(t, extra.selected.execution.matches(afterFull[extra.selected.execution.key]), "33rd Task must retain its exact selecting tuple after all 32 refusals")
	for key, kv := range full {
		require.Equal(t, kv, afterFull[key], key)
	}

	// The stale selecting tuple of the ACK-lost Task cannot reserve another
	// cell or rotate after its unique occupied E has already committed.
	first := groups[0][0]
	require.NoError(t, p.turn(ctx, func(o *workerDueOwner) error {
		if err := p.b.reserveCleanupSlot(o, first.evidence, first.selected); !errors.Is(err, ErrConflict) {
			return fmt.Errorf("same Task stale reserve: %w", err)
		}
		if err := p.b.rotateCleanupSlot(o, first.selected); !errors.Is(err, ErrConflict) {
			return fmt.Errorf("same Task stale rotation: %w", err)
		}
		return nil
	}))
	require.Equal(t, afterFull, cleanupCapacityRows(t, ctx, raw, base.namespace.Root()))

	// The sole deliberate raw E/S mutation is this named ABA experiment.
	// Same bytes at a later S birth must not authenticate the old occupation.
	abas := first.occupied
	_, err = raw.Delete(ctx, abas.slot.key)
	require.NoError(t, err)
	_, err = raw.Put(ctx, abas.slot.key, abas.slot.value)
	require.NoError(t, err)
	require.NoError(t, p.turn(ctx, func(o *workerDueOwner) error {
		_, err := p.b.readCleanupKnownState(o, abas)
		if err == nil {
			return errors.New("same-byte slot ABA accepted")
		}
		return nil
	}))
	beforeRelease := cleanupCapacityRows(t, ctx, raw, base.namespace.Root())
	require.Equal(t, afterFull[abas.execution.key], beforeRelease[abas.execution.key])
	require.Greater(t, beforeRelease[abas.slot.key].CreateRevision, abas.execution.mod)

	for i, peer := range peers {
		t.Logf("metadata peer=%d charged-turns=%d actual-txn=%d conflicts=%d actual-ACK-drops=%d", i, peer.turns, peer.rpcs.Load(), peer.conflicts, peer.dropped.Load())
		require.Zero(t, peer.claimPuts.Load(), "reservation-only scope; not a managed Claim gate proof")
		require.Zero(t, peer.lease.grants.Load(), "observation begins after original session creation")
		require.NoError(t, peer.b.ReleaseWorkerSession(ctx, peer.session))
		unexpected, stop, err := peer.b.beginExpiredTaskOwner(ctx, peer.part, peer.shares[0])
		if unexpected != nil {
			unexpected.finish()
		}
		if stop != nil {
			stop()
		}
		require.Error(t, err)
		require.Error(t, peer.b.rotateCleanupSlot(peer.lastOwner, groups[i][0].selected))
	}
	require.Equal(t, beforeRelease, cleanupCapacityRows(t, ctx, raw, base.namespace.Root()))
	for _, kv := range beforeRelease {
		require.Zero(t, kv.Lease)
	}
}
