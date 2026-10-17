package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func coldQuotaHistoryCursor(t *testing.T, f *coldQuotaHistoryFixture) *coldQuotaWorkerHistory {
	t.Helper()
	state, err := f.b.newColdQuotaWorkerHistory(f.owner, f.workspace, f.claim.reference.IntentID, f.task, f.session, f.part, f.ranges)
	require.NoError(t, err)
	require.NotNil(t, state)
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	return state
}

func coldQuotaHistoryStep(f *coldQuotaHistoryFixture, state *coldQuotaWorkerHistory) (*coldQuotaReturnHistory, error) {
	return f.b.stepColdQuotaWorkerHistory(f.owner, state, f.session, f.part, f.ranges)
}

func coldQuotaHistoryRequests(f *coldQuotaHistoryFixture) []*pb.TxnRequest {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	return append([]*pb.TxnRequest(nil), f.server.requests...)
}

// The expected groups come from the closed loader protocol, independently of
// the cursor. Their shared snapshots must never be split to save a credit.
func coldQuotaHistoryGroups(t *testing.T, f *coldQuotaHistoryFixture) [][]string {
	t.Helper()
	b, intent := f.b, f.claim.reference.IntentID
	j, proof, err := b.namespace.runtimePublicationKeys(f.task.Partition, intent)
	require.NoError(t, err)
	enrollment, activation, err := b.namespace.runtimeEnrollmentKeys(f.task.Partition, intent)
	require.NoError(t, err)
	var record RuntimeEnrolledPublicationRecord
	require.NoError(t, decodeEnrolledRecord(f.server.point(j).Value, &record))
	publicationReceipt, err := b.runtimeDispatchReceiptKey(record.Attempt)
	require.NoError(t, err)
	preparation, err := b.coldHistoricalKeys(f.task.Partition, intent, record)
	require.NoError(t, err)
	reservation, work, err := b.namespace.coldPreparationKeys(f.task.Partition, intent)
	require.NoError(t, err)
	var reserved ColdPreparationRecord
	require.NoError(t, coldDecode(f.server.point(reservation), &reserved))
	settlement, err := b.runtimeDispatchReceiptKey(reserved.Settlement)
	require.NoError(t, err)
	termination, provider, release, err := b.namespace.localRuntimeReleaseKeys(f.task)
	require.NoError(t, err)
	locate := []string{j, proof, enrollment, activation}
	full := append(append([]string{}, locate...), publicationReceipt)
	full = append(full, preparation...)
	return [][]string{locate, full, {reservation, work}, {settlement}, {release}, {termination, provider}, f.historyKeys}
}

// Combining phases, restarting after refusal, charging Claim, losing retained
// fences, or returning a fresh heavy graph at done breaks this progression.
func TestColdQuotaWorkerHistoryPaidPhasesResumeAndPublicEquivalence(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	want, err := f.b.readColdQuotaReturnHistory(f.owner.context, f.workspace, f.claim.reference.IntentID, f.task)
	require.NoError(t, err)
	groups := coldQuotaHistoryGroups(t, f)
	state := coldQuotaHistoryCursor(t, f)
	grants, keeps, revokes := f.server.counts()
	var got *coldQuotaReturnHistory
	for phase, group := range groups {
		f.fund()
		f.b.archiveLife.mu.Lock()
		claimBefore := f.claims.bucket
		f.b.archiveLife.mu.Unlock()
		got, err = coldQuotaHistoryStep(f, state)
		require.NoError(t, err)
		require.Equal(t, phase == 6, got != nil)
		requests := coldQuotaHistoryRequests(f)
		require.Len(t, requests, phase+1)
		require.Equal(t, taskProtoPoints(append([]string{f.b.identityKey, f.b.restoreKey}, group...)), requests[phase].Success)
		require.Equal(t, requests[phase].Success, requests[phase].Failure)
		require.Len(t, requests[phase].Compare, 20)
		f.b.archiveLife.mu.Lock()
		rangeBefore := f.ranges.bucket
		claimAfter := f.claims.bucket
		f.b.archiveLife.mu.Unlock()
		require.Zero(t, rangeBefore.tokens)
		require.Equal(t, claimBefore, claimAfter)
		if phase < 6 {
			entry, refusal := coldQuotaHistoryStep(f, state)
			require.ErrorIs(t, refusal, ErrConflict)
			require.Nil(t, entry)
			require.Len(t, coldQuotaHistoryRequests(f), phase+1)
			f.b.archiveLife.mu.Lock()
			rangeAfter := f.ranges.bucket
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, rangeBefore, rangeAfter)
		}
	}
	locator := want.reservation.Settlement
	require.Equal(t, f.b.coldQuotaReturnRecord(want, locator), f.b.coldQuotaReturnRecord(got, locator))
	require.Equal(t, want.publication.entry.Reference, got.publication.entry.Reference)
	require.Equal(t, want.release.termination, got.release.termination)
	require.Equal(t, []*mvccpb.KeyValue{want.publication.journal, want.publication.receipt, want.reservedKV, want.workKV, want.settledKV, want.release.kv}, []*mvccpb.KeyValue{got.publication.journal, got.publication.receipt, got.reservedKV, got.workKV, got.settledKV, got.release.kv})
	again, err := coldQuotaHistoryStep(f, state)
	require.NoError(t, err)
	require.Same(t, got, again, "done borrows the original immutable shared history")
	require.Len(t, coldQuotaHistoryRequests(f), 7)
	afterGrants, afterKeeps, afterRevokes := f.server.counts()
	require.Equal(t, []int{grants, keeps, revokes}, []int{afterGrants, afterKeeps, afterRevokes})
}

// Removing the located==full comparison would accept the real signed full
// record despite a different canonical record located in the preceding turn.
func TestColdQuotaWorkerHistoryRejectsChangedLocatedRecord(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	groups := coldQuotaHistoryGroups(t, f)
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		if len(request.Success) != 6 {
			return
		}
		point := response.Responses[2].GetResponseRange().Kvs[0]
		var record RuntimeEnrolledPublicationRecord
		require.NoError(t, decodeEnrolledRecord(point.Value, &record))
		record.DataGateEpoch++
		wire, err := encodeEnrolledRecord(record)
		require.NoError(t, err)
		point.Value = []byte(wire)
	}
	state := coldQuotaHistoryCursor(t, f)
	history, err := coldQuotaHistoryStep(f, state)
	require.NoError(t, err)
	require.Nil(t, history)
	f.server.mu.Lock()
	f.server.txnReply = nil
	f.server.mu.Unlock()
	f.fund()
	history, err = coldQuotaHistoryStep(f, state)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, history)
	f.fund()
	history, err = coldQuotaHistoryStep(f, state)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, history)
	requests := coldQuotaHistoryRequests(f)
	require.Len(t, requests, 3)
	require.Equal(t, taskProtoPoints(append([]string{f.b.identityKey, f.b.restoreKey}, groups[1]...)), requests[2].Success)
}

// Empty publication history is the public loader's conflict, while a partial
// group is corruption. Neither failure can create a cached successful result.
func TestColdQuotaWorkerHistoryMissingPublicationKeepsLocate(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-%t", partial), func(t *testing.T) {
			f := newColdQuotaHistoryFixture(t)
			keys := coldQuotaHistoryGroups(t, f)[0]
			original := make([]*mvccpb.KeyValue, len(keys))
			f.server.mu.Lock()
			for i, key := range keys {
				original[i] = workerTestKV(f.server.values[key])
				if !partial || i == 0 {
					delete(f.server.values, key)
				}
			}
			f.server.mu.Unlock()
			state := coldQuotaHistoryCursor(t, f)
			history, err := coldQuotaHistoryStep(f, state)
			want := ErrConflict
			if partial {
				want = ErrCorruptRecord
			}
			require.ErrorIs(t, err, want)
			require.Nil(t, history)
			f.server.mu.Lock()
			for i, key := range keys {
				f.server.values[key] = original[i]
			}
			f.server.mu.Unlock()
			f.fund()
			history, err = coldQuotaHistoryStep(f, state)
			require.NoError(t, err)
			require.Nil(t, history)
			requests := coldQuotaHistoryRequests(f)
			require.Len(t, requests, 2)
			require.Equal(t, requests[0].Success, requests[1].Success)
		})
	}
}

// Every typed phase must fail without publishing partial progress. Restoring
// the same original point retries that phase, never locate or the next phase.
func TestColdQuotaWorkerHistoryTypedFailureRetriesOnlyCurrentPhase(t *testing.T) {
	for phase := 0; phase < 7; phase++ {
		t.Run(fmt.Sprintf("phase-%d", phase+1), func(t *testing.T) {
			f := newColdQuotaHistoryFixture(t)
			groups := coldQuotaHistoryGroups(t, f)
			state := coldQuotaHistoryCursor(t, f)
			for i := 0; i < phase; i++ {
				f.fund()
				history, err := coldQuotaHistoryStep(f, state)
				require.NoError(t, err)
				require.Nil(t, history)
			}
			key := groups[phase][0]
			original := f.server.point(key)
			f.server.mu.Lock()
			f.server.values[key].Value = []byte("malformed history")
			f.server.mu.Unlock()
			f.fund()
			history, err := coldQuotaHistoryStep(f, state)
			require.Error(t, err)
			require.Nil(t, history)
			f.server.mu.Lock()
			f.server.values[key] = original
			f.server.mu.Unlock()
			f.fund()
			history, err = coldQuotaHistoryStep(f, state)
			require.NoError(t, err)
			require.Equal(t, phase == 6, history != nil)
			requests := coldQuotaHistoryRequests(f)
			require.Len(t, requests, phase+2)
			require.Equal(t, requests[phase].Success, requests[phase+1].Success)
		})
	}
}

// A canonical release rebuilt for a corrupted signature must still fail at
// signed history, rather than relying only on the release's byte equality.
func TestColdQuotaWorkerHistoryAuthenticatesTerminalSignature(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	r := f.termination
	var envelope struct {
		Claims    p.NodeTerminationProofClaims `json:"claims"`
		Signature []byte                       `json:"signature"`
	}
	require.NoError(t, json.Unmarshal(r.TerminalProof, &envelope))
	envelope.Signature[0] ^= 1
	wire, err := json.Marshal(envelope)
	require.NoError(t, err)
	r.TerminalProof = wire
	terminal, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(f.keys.Intent, string(terminal), 0)
	intent := f.server.point(f.keys.Intent)
	old, err := decodeLocalOwnerRelease(f.server.point(f.releaseKey))
	require.NoError(t, err)
	release, err := newLocalOwnerReleaseRecord(r, intent.CreateRevision, intent.ModRevision, old.Claim)
	require.NoError(t, err)
	releaseWire, err := encodeLocalOwnerRelease(release)
	require.NoError(t, err)
	_, err = f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(f.releaseKey)).Commit()
	require.NoError(t, err)
	f.server.put(f.releaseKey, releaseWire, 0)
	state := coldQuotaHistoryCursor(t, f)
	for phase := 0; phase < 6; phase++ {
		f.fund()
		history, err := coldQuotaHistoryStep(f, state)
		require.NoError(t, err)
		require.Nil(t, history)
	}
	f.fund()
	history, err := coldQuotaHistoryStep(f, state)
	require.Error(t, err)
	require.Nil(t, history)
	require.Len(t, coldQuotaHistoryRequests(f), 7)
}

// Constructor and every later phase, including cached done, must reject copied
// or substituted membership before admission. Credit cannot repair rebinding.
func TestColdQuotaWorkerHistoryRejectsRebindingBeforeRead(t *testing.T) {
	for _, progress := range []int{0, 3, 7} {
		for _, fault := range []string{"state-copy", "owner-copy", "context", "session-copy", "partition-copy", "range-copy", "finished"} {
			t.Run(fmt.Sprintf("progress-%d/%s", progress, fault), func(t *testing.T) {
				f := newColdQuotaHistoryFixture(t)
				state := coldQuotaHistoryCursor(t, f)
				for i := 0; i < progress; i++ {
					f.fund()
					_, err := coldQuotaHistoryStep(f, state)
					require.NoError(t, err)
				}
				owner, session, part, ranges := f.owner, f.session, f.part, f.ranges
				switch fault {
				case "state-copy":
					copy := *state
					state = &copy
				case "owner-copy":
					copy := *owner
					owner = &copy
				case "context":
					owner.context = context.Background()
				case "session-copy":
					copy := *session
					session = &copy
				case "partition-copy":
					copy := *part
					part = &copy
				case "range-copy":
					copy := *ranges
					ranges = &copy
				case "finished":
					owner.finish()
				}
				f.fund()
				before := len(coldQuotaHistoryRequests(f))
				f.b.archiveLife.mu.Lock()
				rangeBefore, claimBefore := f.ranges.bucket, f.claims.bucket
				f.b.archiveLife.mu.Unlock()
				history, err := f.b.stepColdQuotaWorkerHistory(owner, state, session, part, ranges)
				require.Error(t, err)
				require.Nil(t, history)
				require.Len(t, coldQuotaHistoryRequests(f), before)
				f.b.archiveLife.mu.Lock()
				rangeAfter, claimAfter := f.ranges.bucket, f.claims.bucket
				f.b.archiveLife.mu.Unlock()
				require.Equal(t, rangeBefore, rangeAfter)
				require.Equal(t, claimBefore, claimAfter)
			})
		}
	}
}

// Invalid public-domain inputs must fail locally before a cursor or paid read
// exists, retaining the public authenticate entry's original validation order.
func TestColdQuotaWorkerHistoryConstructorValidation(t *testing.T) {
	for _, fault := range []string{"release-config", "workspace", "task", "partition", "profile", "owner-copy", "session-copy"} {
		t.Run(fault, func(t *testing.T) {
			f := newColdQuotaHistoryFixture(t)
			owner, session, w, task := f.owner, f.session, f.workspace, f.task
			switch fault {
			case "release-config":
				f.b.localRuntimeRelease = nil
			case "workspace":
				w = WorkspaceIdentity{}
			case "task":
				task.TaskID = "invalid"
			case "partition":
				task.Partition++
			case "profile":
				f.b.preparationInventory = nil
			case "owner-copy":
				copy := *owner
				owner = &copy
			case "session-copy":
				copy := *session
				session = &copy
			}
			before := len(coldQuotaHistoryRequests(f))
			state, err := f.b.newColdQuotaWorkerHistory(owner, w, f.claim.reference.IntentID, task, session, f.part, f.ranges)
			require.Error(t, err)
			require.Nil(t, state)
			require.Len(t, coldQuotaHistoryRequests(f), before)
		})
	}
}

// One malformed native ACK loses the original session; a new credit cannot
// advance or dispatch another old-session history call.
func TestColdQuotaWorkerHistoryNativeUnknownDoesNotResume(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	state := coldQuotaHistoryCursor(t, f)
	f.server.txnReply = func(_ *pb.TxnRequest, response *pb.TxnResponse) { response.Header = nil }
	history, err := coldQuotaHistoryStep(f, state)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Nil(t, history)
	f.fund()
	history, err = coldQuotaHistoryStep(f, state)
	require.Error(t, err)
	require.Nil(t, history)
	require.Len(t, coldQuotaHistoryRequests(f), 1)
	f.server.mu.Lock()
	f.server.txnReply = nil
	f.server.mu.Unlock()
}

// Business caps remain narrower than the transport envelope. Each phase's
// largest source is bounded before typed parsing and invalidates its session
// on an actual overrun, rather than making the malformed response retryable.
func TestColdQuotaWorkerHistoryBusinessCaps(t *testing.T) {
	for phase, source := range []struct {
		index int
		limit int
	}{{1, 16384}, {6, 65536}, {0, 16384}, {0, 4096}, {0, 8192}, {0, 16384}, {7, 65536}} {
		t.Run(fmt.Sprintf("phase-%d", phase+1), func(t *testing.T) {
			f := newColdQuotaHistoryFixture(t)
			groups := coldQuotaHistoryGroups(t, f)
			state := coldQuotaHistoryCursor(t, f)
			for i := 0; i < phase; i++ {
				f.fund()
				_, err := coldQuotaHistoryStep(f, state)
				require.NoError(t, err)
			}
			f.server.mu.Lock()
			f.server.values[groups[phase][source.index]].Value = []byte(strings.Repeat("x", source.limit+1))
			f.server.mu.Unlock()
			f.fund()
			history, err := coldQuotaHistoryStep(f, state)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Nil(t, history)
			require.Len(t, coldQuotaHistoryRequests(f), phase+1)
		})
	}
}

// The publication fixture owns its creation-claim cleanup. This independent
// cold reader lets Close own only the cursor's actual original native call.
func coldQuotaHistoryIndependentReader(t *testing.T, source *coldQuotaHistoryFixture) *coldQuotaHistoryFixture {
	t.Helper()
	require.NoError(t, source.b.ReleaseWorkerSession(context.Background(), source.session))
	source.owner.finish()
	f := *source
	var identity Identity
	require.NoError(t, json.Unmarshal([]byte(source.b.identityValue), &identity))
	// RestoreEpoch is deliberately excluded from identity JSON; the original
	// backend authenticated it from its separate operator metadata point.
	identity.RestoreEpoch = source.b.restoreEpoch
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	// Match workerTestEndpoint's explicit opt-in for this loopback host model.
	b, err := New(context.Background(), Options{Endpoints: source.b.client.Endpoints(), Namespace: source.b.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: source.b.requestTimeout, WorkerControl: &WorkerControlOptions{}, PublicationTrust: &RuntimePublicationTrust{AuthorityID: source.b.publicationAuthorityID, Target: source.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: goodAuthorityClock(), EnrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	f.duePublicationFixture.b = b
	b.preparationInventory, b.preparationDigest, b.preparationTotal = source.b.preparationInventory, source.b.preparationDigest, source.b.preparationTotal
	b.localRuntimeRelease = source.b.localRuntimeRelease
	f.owner, err = b.registerEnrolledPublication(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-f.owner.done:
		default:
			f.owner.finish()
		}
	})
	f.session, err = b.OpenWorkerSession(context.Background(), "cold-history-close")
	require.NoError(t, err)
	f.part, err = b.ClaimWorkerPartition(context.Background(), f.session, f.task.Partition)
	require.NoError(t, err)
	f.ranges, err = b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	f.claims, err = b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	f.fund()
	return &f
}

// Replacing the owner context after the real return must not publish decoded
// partial state. Restoring the original context retries the same paid phase.
func TestColdQuotaWorkerHistoryRechecksBindingBeforePublishingPhase(t *testing.T) {
	f := coldQuotaHistoryIndependentReader(t, newColdQuotaHistoryFixture(t))
	state := coldQuotaHistoryCursor(t, f)
	original := f.owner.context
	changed := false
	coldQuotaMetadataIntercept(t, &coldQuotaAdmissionFixture{b: f.b, server: f.server, owner: f.owner, session: f.session, part: f.part, ranges: f.ranges, claims: f.claims}, func(_ context.Context, _ *pb.TxnResponse, err error) error {
		if !changed {
			changed = true
			f.owner.context = context.Background()
		}
		return err
	})
	history, err := coldQuotaHistoryStep(f, state)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, history)
	f.owner.context = original
	f.fund()
	history, err = coldQuotaHistoryStep(f, state)
	require.NoError(t, err)
	require.Nil(t, history)
	requests := coldQuotaHistoryRequests(f)
	require.Len(t, requests, 2)
	require.Equal(t, requests[0].Success, requests[1].Success)
}

// Cancellation must not release cursor busy ownership or complete Close before
// the intercepted original client return. No second phase may escape that join.
func TestColdQuotaWorkerHistoryCloseJoinsActualReturn(t *testing.T) {
	f := coldQuotaHistoryIndependentReader(t, newColdQuotaHistoryFixture(t))
	state := coldQuotaHistoryCursor(t, f)
	entered, allow := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(allow) })
	coldQuotaMetadataIntercept(t, &coldQuotaAdmissionFixture{b: f.b, server: f.server, owner: f.owner, session: f.session, part: f.part, ranges: f.ranges, claims: f.claims}, func(_ context.Context, _ *pb.TxnResponse, err error) error {
		close(entered)
		<-allow
		return err
	})
	finished := make(chan error, 1)
	go func() {
		_, err := coldQuotaHistoryStep(f, state)
		f.owner.finish()
		finished <- err
	}()
	coldQuotaJournalAwait(t, entered)
	f.b.archiveLife.mu.Lock()
	call := f.session.operation
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, call)
	history, err := coldQuotaHistoryStep(f, state)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, history)
	closed := make(chan error, 1)
	go func() { closed <- f.b.Close() }()
	coldQuotaJournalAwait(t, call.ctx.Done())
	select {
	case <-call.done:
		t.Fatal("history call finished before actual native return")
	default:
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before original history client: %v", err)
	default:
	}
	release.Do(func() { close(allow) })
	require.ErrorIs(t, workerTestReceive(t, finished), ErrOutcomeUnknown)
	require.NoError(t, workerTestReceive(t, closed))
	require.Len(t, coldQuotaHistoryRequests(f), 1)
}
