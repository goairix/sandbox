package etcd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The host fixture has no enrolled release configuration. It can authenticate
// birth and preliminary release bytes, but cannot authenticate a release seal.
// Configured execution and actual-return joins require a genuine Linux model
// fixture: New rejects archive recovery on other platforms. No live backend
// configuration, session TTL or lease deadline is modified here.
func cleanupBirthReleaseTestObservation(t *testing.T, f *backfillBootstrapFixture) *cleanupBirthEvidence {
	t.Helper()
	s := seedCleanupBirth(t, f)
	f.server.put(f.b.namespace.Root()+"release-test-revision", "advance", 0)
	f.server.put(s.keys[6], cleanupBirthTestRelease(t, s), 0)
	birth := cleanupBirthTestRead(t, f, s.task.Reference.TaskID)
	require.NotNil(t, birth.release)
	require.NotNil(t, birth.releaseRecord)
	return birth
}

func TestCleanupBirthReleaseAdmission(t *testing.T) {
	f := newBackfillBootstrapFixture(t, 60*time.Second)
	birth := cleanupBirthReleaseTestObservation(t, f)
	other := newBackfillBootstrapFixture(t, 60*time.Second)
	foreign := cleanupBirthReleaseTestObservation(t, other)
	copied := *birth
	copiedPartition := *f.partition
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	backfillBootstrapCredit(f.b, f.ranges, 262144)
	before := len(backfillBootstrapRequests(f))
	for _, tc := range []struct {
		name      string
		backend   *Backend
		ctx       context.Context
		partition *WorkerPartitionClaim
		share     *WorkerBudgetShare
		birth     *cleanupBirthEvidence
	}{
		{"nil-context", f.b, nil, f.partition, f.ranges, birth},
		{"nil-backend", nil, context.Background(), f.partition, f.ranges, birth},
		{"nil-birth", f.b, context.Background(), f.partition, f.ranges, nil},
		{"copied-birth", f.b, context.Background(), f.partition, f.ranges, &copied},
		{"foreign-birth", f.b, context.Background(), f.partition, f.ranges, foreign},
		{"copied-partition", f.b, context.Background(), &copiedPartition, f.ranges, birth},
		{"foreign-range", f.b, context.Background(), f.partition, other.ranges, birth},
		{"claim-budget", f.b, context.Background(), f.partition, f.claims, birth},
		{"canceled", f.b, canceled, f.partition, f.ranges, birth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof, err := tc.backend.readCleanupBirthRelease(tc.ctx, tc.partition, tc.share, tc.birth)
			require.Error(t, err)
			require.Nil(t, proof)
		})
	}

	// Even authentic birth plus preliminary release cannot replace the genuine
	// constructor's enrollment/verifier/provider/recovery prerequisites. The
	// cases above assert refusal only; isolated pointer validation still needs
	// the configured Linux fixture so missing configuration cannot mask it.
	proof, err := f.b.readCleanupBirthRelease(context.Background(), f.partition, f.ranges, birth)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, proof)
	require.Len(t, backfillBootstrapRequests(f), before)
	f.b.archiveLife.mu.Lock()
	tokens, owner, liveErr := f.ranges.bucket.tokens, f.ranges.dueOwner, f.session.live()
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, uint64(262144), tokens)
	require.Nil(t, owner)
	require.NoError(t, liveErr)
}

type cleanupBirthReleaseTransportCause struct{ message string }

func (e *cleanupBirthReleaseTransportCause) Error() string { return e.message }

func TestCleanupBirthReleaseTransport(t *testing.T) {
	t.Run("original-reader-marks-rpc-error-without-changing-cause", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		cause := &cleanupBirthReleaseTransportCause{message: "injected native transport failure"}
		f.b.client.KV = &faultKV{
			KV: f.b.client.KV,
			match: func(ops []clientv3.Op) bool {
				return len(ops) == 3 && ops[0].IsGet()
			},
			after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				return r, cause
			},
		}
		key := f.b.namespace.Root() + "release-transport-test"
		points, err := f.b.readEnrolledPoints(context.Background(), []string{key}, []int{8192})
		require.Nil(t, points)
		require.Error(t, err)
		require.Equal(t, cause.Error(), err.Error())
		require.ErrorIs(t, err, cause)
		var marker *enrolledPointTransportError
		require.ErrorAs(t, err, &marker)
		var observed *cleanupBirthReleaseTransportCause
		require.ErrorAs(t, err, &observed)
		require.Same(t, cause, observed)

		// The original proof loader may join transport failure with corruption.
		// Its outer validation sentinel must never erase certainty information.
		joined := errors.Join(ErrCorruptRecord, fmt.Errorf("proof history: %w", err))
		require.ErrorIs(t, joined, ErrCorruptRecord)
		require.ErrorIs(t, joined, cause)
		require.ErrorAs(t, joined, &marker)
		require.False(t, cleanupBirthReleaseValidationError(joined))
	})
	t.Run("original-reader-marks-context-error", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		points, err := f.b.readEnrolledPoints(ctx, []string{f.b.namespace.Root() + "release-context-test"}, []int{8192})
		require.Nil(t, points)
		require.ErrorIs(t, err, context.Canceled)
		var marker *enrolledPointTransportError
		require.ErrorAs(t, err, &marker)
		require.False(t, cleanupBirthReleaseValidationError(err))
	})
	t.Run("native-envelope-failure-is-not-marked-as-transport", func(t *testing.T) {
		f := newBackfillBootstrapFixture(t, 60*time.Second)
		f.b.client.KV = &faultKV{
			KV: f.b.client.KV,
			match: func(ops []clientv3.Op) bool {
				return len(ops) == 3 && ops[0].IsGet()
			},
			after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				r.Responses[2].GetResponseRange().More = true
				return r, nil
			},
		}
		points, err := f.b.readEnrolledPoints(context.Background(), []string{f.b.namespace.Root() + "release-envelope-test"}, []int{8192})
		require.Nil(t, points)
		require.Error(t, err)
		var marker *enrolledPointTransportError
		require.False(t, errors.As(err, &marker), "the annotation changes only the native RPC/context error branch")
	})
}

// This pure predicate classifies only whether a refusal may attempt the final
// guard audit. True grants no proof or healthy-session result: the actual
// wrapper must still perform the original postguard and owner.postcheck.
func TestCleanupBirthReleaseRefusal(t *testing.T) {
	validation := fmt.Errorf("invalid signed terminal proof")
	transport := &enrolledPointTransportError{cause: &cleanupBirthReleaseTransportCause{message: "network failure"}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil-proof", nil, true},
		{"corrupt-record", ErrCorruptRecord, true},
		{"corrupt-receipt", ErrCorruptReceipt, true},
		{"signature-validation-leaf", errors.Join(ErrCorruptRecord, validation), true},
		{"unrecognized-leaf", validation, false},
		{"invalid-configuration", ErrInvalidConfiguration, false},
		{"transport", transport, false},
		{"nested-joined-transport", errors.Join(ErrCorruptRecord, fmt.Errorf("nested: %w", transport)), false},
		{"outcome-unknown", errors.Join(ErrCorruptRecord, ErrOutcomeUnknown), false},
		{"identity", errors.Join(ErrCorruptRecord, ErrIdentityMismatch), false},
		{"guard", errors.Join(ErrCorruptReceipt, ErrGuardExpired), false},
		{"canceled", errors.Join(ErrCorruptRecord, context.Canceled), false},
		{"deadline", errors.Join(ErrCorruptRecord, context.DeadlineExceeded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, cleanupBirthReleaseValidationError(tc.err))
		})
	}
}
