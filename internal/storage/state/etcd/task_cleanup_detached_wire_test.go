package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The producers and bundle read are real. The issuer revision and worker
// dimensions are explicitly codec-only: this host has no genuine Kernel and
// the model is never passed to a managed Claim or runtime effect API.
func cleanupDetachedWireFixture(t *testing.T) (*Backend, cleanupDetachedWireInput, cleanupLocalWireDimensions) {
	t.Helper()
	f := newDetachedCleanupSourceFixture(t, "detached", &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest})
	bundle, err := f.b.loadTaskClaimBundle(context.Background(), f.task.Reference)
	require.NoError(t, err)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	verifier, err := p.NewManagementVerifier(p.TrustBinding{Namespace: f.b.namespace.Root(), AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, RestoreEpoch: f.b.restoreEpoch}, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	identity, err := verifier.VerifyCommandIssuerCertificate(f.enrollment.Request.IssuerCertificate, f.now)
	require.NoError(t, err)
	issuer := CommandIssuerEntry{Revision: math.MaxInt64, Record: CommandIssuerRecord{Version: 1, Namespace: f.b.namespace.Root(), RestoreEpoch: f.b.restoreEpoch, CertificateID: identity.CertificateID(), CertificateDigest: identity.Digest(), Certificate: identity.Wire()}}
	require.NoError(t, issuer.Record.Validate())
	_, model := cleanupLocalWireFixture(t)
	dimensions := cleanupLocalWireDimensions{runID: "11111111-1111-4111-8111-111111111111", claimID: "22222222-2222-4222-8222-222222222222", worker: model.state.record.Run.Worker}
	return f.b, cleanupDetachedWireInput{bundle: bundle, snapshot: f.snapshot, intent: f.intent, workspace: f.claim.workspace, publication: &f.publication, issuer: issuer, workerID: "detached-wire", claimTTL: 60 * time.Second}, dimensions
}

// This is an independent protocol inventory, including aliases for native
// calls at different recovery states. Every retained-history subread is named;
// a family count cannot conceal missing none/cold13/actualTask variants.
func cleanupDetachedWireExpected() map[string][]string {
	want := map[string][]string{
		"source":            strings.Fields("snapshot-placement creation-intent publication-initial publication-complete"),
		"stage-begin":       strings.Fields("generic-begin stage-grant stage-revoke"),
		"stage-commit":      strings.Fields("close-commit checkpoint-commit"),
		"stage-resolve":     strings.Fields("close-resolve checkpoint-resolve quiescence-resolve"),
		"quiescence-stage":  strings.Fields("reserved-begin reserved-commit"),
		"task-checkpoint":   strings.Fields("claim-create claim-post-fence checkpoint-mutation"),
		"task-renew":        strings.Fields("renew-before renew-after task-grant task-keepalive"),
		"task-release":      strings.Fields("task-revoke"),
		"close":             strings.Fields("issuer-read issuer-register close-pre-guard close-committed-guard close-intent-read close-stage-receipt-read"),
		"close-query":       strings.Fields("query-close-history query-close-stage-receipt query-close-issuer"),
		"quiescence":        strings.Fields("reservation-read reservation-write quiescence-pre-guard quiescence-committed-guard quiescence-intent-read.initial quiescence-intent-read.prior-initial quiescence-intent-read.prior-complete quiescence-receipt-read"),
		"quiescence-query":  strings.Fields("query-quiescence-history.initial query-quiescence-history.prior-initial query-quiescence-history.prior-complete query-quiescence-stage-receipt query-quiescence-issuer"),
		"dual-drain":        strings.Fields("operations-page empty-before empty-after"),
		"final-sync-none":   strings.Fields("final-sync-read final-sync-reserve"),
		"execution":         strings.Fields("point bundle observe claim-observe initialize rotate reserve progress claim claim-postcheck joined settle terminal-history"),
		"detached-noeffect": strings.Fields("locate quiescence source arbitration write"),
		"detached-recovery": strings.Fields("early-history early-new-run new-run arbitration"),
		"termination":       strings.Fields("authority-history-read.domain authority-history-read.publication-initial authority-history-read.publication-complete authority-history-read.none-locate authority-history-read.none-quiescence authority-history-read.none-source authority-history-read.supplement termination-read provider-read reserve reserved-guard terminated-write terminated-guard cleanup-complete-write cleanup-complete-guard"),
		"owner-release":     strings.Fields("release-proof-read release-history-read.head-with-task release-history-read.head-paid-task release-history-read.retained13 release release-arbitration"),
		"provider":          strings.Fields("provider-initial provider-before-termination provider-after-termination"),
	}
	for step := 0; step < 7; step++ {
		for _, phase := range []string{"intent", "unknown", "complete", "guard"} {
			want["provider"] = append(want["provider"], fmt.Sprintf("step-%d-%s", step, phase))
		}
		if step >= 2 && step <= 5 {
			want["provider"] = append(want["provider"], fmt.Sprintf("step-%d-skipped", step))
		}
	}
	return want
}

func TestCleanupDetachedWireCompleteNativeManifest(t *testing.T) {
	b, input, dimensions := cleanupDetachedWireFixture(t)
	require.Len(t, taskRuntimeCleanupSteps, 7)
	want := cleanupDetachedWireExpected()
	seen := make(map[string]bool)
	nodes := map[string]int{"detached-noeffect/locate": 12, "detached-noeffect/quiescence": 14, "detached-noeffect/source": 22, "detached-noeffect/arbitration": 10, "detached-noeffect/write": 64, "detached-recovery/new-run": 51, "owner-release/release": 63, "owner-release/release-proof-read": 10, "owner-release/release-history-read.head-with-task": 14, "owner-release/release-history-read.head-paid-task": 12, "owner-release/release-history-read.retained13": 34}
	report, err := b.visitCleanupDetachedWire(input, dimensions, func(emission cleanupDetachedWireEmission) error {
		key := emission.family + "/" + emission.variant
		require.Contains(t, want, emission.family)
		require.Contains(t, want[emission.family], emission.variant)
		require.False(t, seen[key], "duplicate native variant %s", key)
		seen[key] = true
		require.NotEqual(t, emission.txn == nil, emission.lease == nil, "exactly one native RPC type")
		if emission.txn != nil {
			projection := *emission.txn
			require.NoError(t, validateCleanupWire(projection), key)
			actual := len(projection.request.Compare) + len(projection.request.Success) + len(projection.request.Failure)
			if wantNodes, ok := nodes[key]; ok {
				require.Equal(t, wantNodes, actual, key)
			}
			require.LessOrEqual(t, actual, 64, key)
			require.LessOrEqual(t, projection.request.Size(), 128<<10, key)
			require.LessOrEqual(t, projection.success.Size(), 256<<10, key)
			require.LessOrEqual(t, projection.failure.Size(), 256<<10, key)
		} else {
			require.NoError(t, validateCleanupLeaseWire(*emission.lease), key)
		}
		return nil
	})
	require.NoError(t, err, "complete detached emitter must reuse real native builders")
	count := 0
	for family, variants := range want {
		for _, variant := range variants {
			count++
			require.True(t, seen[family+"/"+variant], "missing %s/%s", family, variant)
		}
	}
	require.True(t, report.complete)
	require.Equal(t, count, report.variants)
	require.LessOrEqual(t, report.retainedKeys, 84, "actual source/bundle/state/current-projection key union")
}

func TestCleanupDetachedWireSingleStageMatchesOriginalNativeBuilder(t *testing.T) {
	b, input, dimensions := cleanupDetachedWireFixture(t)
	ref := input.bundle.initial.reference(cleanupWirePaddingDigest())
	ref.StageID, ref.RequestID, ref.AttemptID = "task_quiesce_users", dimensions.claimID, dimensions.runID
	guardKey, receiptKey, err := b.stageKeys(ref)
	require.NoError(t, err)
	key, err := b.namespace.taskQuiescenceKey(input.bundle.task.Reference)
	require.NoError(t, err)
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	require.NoError(t, err)
	mutation := Mutation{Comparisons: []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)}, Writes: []Write{{Key: key, Value: make([]byte, 32768)}}}
	reservation, err := b.namespace.taskQuiescenceAttemptKey(input.bundle.task.Reference)
	require.NoError(t, err)
	for _, pin := range []*taskQuiescenceReservationPin{nil, {key: reservation, revision: math.MaxInt64}} {
		original, err := b.taskStageWireProjections(ref, mutation, guardKey, receiptKey, committed, pin)
		require.NoError(t, err)
		for i, variant := range []string{"begin", "commit", "resolve"} {
			one, err := b.taskStageWireProjection(variant, ref, mutation, guardKey, receiptKey, committed, pin)
			require.NoError(t, err, variant)
			require.Equal(t, original[i], one, "single-variant extraction must preserve all native branches/options")
		}
	}
}

func TestCleanupDetachedWireVisitorStopsBeforeNextVariant(t *testing.T) {
	b, input, dimensions := cleanupDetachedWireFixture(t)
	stopped := errors.New("original visitor stopped")
	count := 0
	report, err := b.visitCleanupDetachedWire(input, dimensions, func(cleanupDetachedWireEmission) error {
		count++
		return stopped
	})
	require.ErrorIs(t, err, stopped)
	require.Equal(t, 1, count, "producer cannot build/emit later variants after refusal")
	require.False(t, report.complete)
}

func TestCleanupDetachedWireAllocationRefusesBeforeMaterialization(t *testing.T) {
	require.NoError(t, checkCleanupDetachedWireAllocation(cleanupDetachedWireAllocation{keyBytes: 64 * 1024, valueBytes: 128 << 10, schemaBytes: 176128, nodes: 64, keys: 64}))
	for _, allocation := range []cleanupDetachedWireAllocation{
		{keyBytes: math.MaxUint64, valueBytes: 1, nodes: 64, keys: 64},
		{keyBytes: 1, valueBytes: math.MaxUint64, nodes: 64, keys: 64},
		{keyBytes: 1024, valueBytes: 1, schemaBytes: math.MaxUint64, nodes: 64, keys: 64},
		{keyBytes: 1024, valueBytes: 1, nodes: 65, keys: 64},
	} {
		var err error
		allocations := testing.AllocsPerRun(10, func() { err = checkCleanupDetachedWireAllocation(allocation) })
		require.Error(t, err)
		require.Zero(t, allocations, "scalar overflow/cap refusal must not allocate padding or protobufs")
	}
}
