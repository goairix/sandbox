package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"sync"
	"testing"
	"time"
)

type archiveNativeIssuer struct {
	old                  *taskCloseProvider
	certCalls, signCalls int
	certificate          func(context.Context) ([]byte, error)
	sign                 func(context.Context, string, p.TaskWorkspaceArchiveTicketClaims) ([]byte, error)
}

// Shared fixtures register cleanup before their claims are intentionally
// released for the next Backend. Avoid only those exact duplicate revokes;
// all first revokes and unexpected errors remain genuine native calls.
type archiveCleanupLease struct {
	clientv3.Lease
	mu       sync.Mutex
	released map[clientv3.LeaseID]bool
}

func (l *archiveCleanupLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released[id] {
		return nil, rpctypes.ErrLeaseNotFound
	}
	r, err := l.Lease.Revoke(ctx, id)
	if err == nil {
		l.released[id] = true
	}
	return r, err
}
func archiveTrackCleanup(b *Backend) {
	b.client.Lease = &archiveCleanupLease{Lease: b.client.Lease, released: map[clientv3.LeaseID]bool{}}
}
func (a *archiveNativeIssuer) Certificate(ctx context.Context) ([]byte, error) {
	a.certCalls++
	if a.certificate != nil {
		return a.certificate(ctx)
	}
	return bytes.Clone(a.old.wire), nil
}
func (a *archiveNativeIssuer) SignWorkspaceArchive(ctx context.Context, digest string, claims p.TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
	a.signCalls++
	if a.sign != nil {
		return a.sign(ctx, digest, claims)
	}
	if digest != a.old.identity.Digest() {
		return nil, ErrIdentityMismatch
	}
	return p.SignTaskWorkspaceArchiveTicket(a.old.key, a.old.identity, claims)
}

// Configure a new task-local Backend before its new original claim/preparation;
// the accepted native fixture and its pinned TLS/runtime definitions stay intact.
func archiveNativeFixture(t *testing.T) (*quiescenceNativeFixture, *archiveNativeIssuer, *PreparedTaskQuiescence, *quiescenceWireTarget) {
	t.Helper()
	base := newTaskDeliveryFixture(t, true)
	archiveTrackCleanup(base.b)
	f := finishQuiescenceFixture(t, base)
	archiveTrackCleanup(f.b)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
	identity, err := decodeIdentity(f.b.identityValue, f.b.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = f.b.restoreEpoch
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	a := &archiveNativeIssuer{old: f.issuer.old}
	clockAnchor := time.Now()
	b, err := New(f.ctx, Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, TaskQuiescenceIssuer: f.issuer, TaskWorkspaceArchiveIssuer: a, PublicationTrust: &RuntimePublicationTrust{AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: f.now.Add(time.Since(clockAnchor))}, nil
	})})
	require.NoError(t, err)
	archiveTrackCleanup(b)
	require.Zero(t, a.certCalls, "New must not call the issuer")
	require.Zero(t, a.signCalls, "New must not call the signer")
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	c, err := b.ClaimTask(f.ctx, f.c.reference.Task, "archive-worker", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { taskReleaseClaim(t, b, c) })
	f.b, f.c = b, c
	prepared, target := finalizationNativePrepared(t, f)
	r, err := b.ReserveTaskFinalSync(f.ctx, c, prepared, target.destination, finalSyncPlan())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, r.Outcome)
	return f, a, prepared, target
}

func archiveNativeCapture(t *testing.T, f *quiescenceNativeFixture, before func(context.Context) error, after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)) *int {
	t.Helper()
	original := f.b.client.KV
	key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
	require.NoError(t, err)
	writes := new(int)
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		require.LessOrEqual(t, len(cmps)+len(yes)+len(no), maxStageOperations)
		writing := putsKey(key)(yes)
		if writing {
			*writes++
			d := f.c.finalSyncDraft.archive
			require.True(t, d.writeAttempted)
			require.Empty(t, yes[0].RangeBytes())
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.False(t, deadline.After(d.prepared.deadline))
			keys := []string{f.b.identityKey, f.b.restoreKey, key}
			r := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(key, d.value, 0)}, Failure: taskProtoPoints(keys)}
			require.LessOrEqual(t, r.Size(), maxMutationBytes)
			t.Logf("archive_command nodes=%d write_bytes=%d command_bytes=%d", len(cmps)+len(yes)+len(no), r.Size(), len(d.value))
			if before != nil {
				if err := before(ctx); err != nil {
					return nil, err
				}
			}
		}
		r, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if writing && after != nil {
			return after(r, err)
		}
		return r, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	return writes
}

func TestTaskArchiveCommandNativeOriginal(t *testing.T) {
	f, a, prepared, target := archiveNativeFixture(t)
	writes := archiveNativeCapture(t, f, nil, nil)
	queries := target.queries.Load()
	r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, r.Outcome)
	require.NotNil(t, r.Prepared)
	require.True(t, f.b.validPreparedTaskWorkspaceArchive(r.Prepared))
	d := f.c.finalSyncDraft.archive
	require.Equal(t, f.c.finalSyncDraft.record.AttemptID, r.Reference.CommandID)
	require.Equal(t, d.finalSync.revision, r.Reference.FinalSyncRevision)
	require.Equal(t, prepared.receipt.Context(), d.claims.Context.QuiescenceContext)
	require.Equal(t, digestTaskQuiescenceProof(prepared.receipt.Wire()), d.claims.Context.QuiescenceReceiptDigest)
	require.Equal(t, 21*time.Second, d.claims.NotAfter.Sub(d.claims.NotBefore))
	require.Equal(t, 45*time.Second, d.claims.SessionNotAfter.Sub(d.claims.NotBefore))
	require.Equal(t, prepared.deadline, d.deadline)
	require.Greater(t, target.queries.Load(), queries)
	key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
	require.NoError(t, err)
	native, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, native.Kvs, 1)
	var record taskArchiveCommandRecord
	require.NoError(t, decodeTaskArchiveCommand(native.Kvs[0], &record))
	require.Equal(t, d.claims, record.Claims)
	require.Equal(t, prepared.receipt.Wire(), []byte(record.QuiescenceReceipt))
	require.EqualValues(t, 1, native.Kvs[0].Version)
	require.Zero(t, native.Kvs[0].Lease)
	require.Equal(t, native.Kvs[0].CreateRevision, native.Kvs[0].ModRevision)
	require.Greater(t, native.Kvs[0].CreateRevision, d.finalSync.revision)
	copyHandle := *r.Prepared
	require.False(t, f.b.validPreparedTaskWorkspaceArchive(&copyHandle))
	copyPrerequisite := *prepared
	x, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, &copyPrerequisite, target.destination, 45*time.Second)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, x.Prepared)
	before := d.claims
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	again, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
	require.NoError(t, err)
	require.Same(t, r.Prepared, again.Prepared)
	require.Equal(t, before, d.claims)
	require.Equal(t, 1, *writes)
	require.Equal(t, 1, a.certCalls)
	require.Equal(t, 1, a.signCalls)
	require.Zero(t, target.effects.Load())
	cold := quiescenceColdBackend(t, f, false)
	ref, err := cold.LoadTaskWorkspaceArchive(f.ctx, f.c.reference.Task)
	require.NoError(t, err)
	require.Equal(t, r.Reference, *ref)
	x, err = cold.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
	require.Error(t, err)
	require.Nil(t, x.Prepared)
}

func TestTaskArchiveCommandNativeUnknownCannotPrepare(t *testing.T) {
	for _, fault := range []string{"committed", "absent", "canceled", "claim-lost", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			f, a, prepared, target := archiveNativeFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			before := func(context.Context) error {
				if fault == "absent" {
					return errors.New("archive response absent")
				}
				return nil
			}
			after := func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
				require.NoError(t, e)
				require.True(t, r.Succeeded)
				if fault == "canceled" {
					cancel()
				}
				if fault == "claim-lost" {
					_, err := f.raw.Delete(f.ctx, f.c.guardKey)
					require.NoError(t, err)
				}
				if fault == "malformed" {
					r.Responses = nil
					return r, nil
				}
				return nil, errors.New("archive reply lost")
			}
			writes := archiveNativeCapture(t, f, before, after)
			first, err := f.b.PrepareTaskWorkspaceArchive(ctx, f.c, prepared, target.destination, 45*time.Second)
			require.Error(t, err)
			require.Nil(t, first.Prepared)
			for i := 0; i < 2; i++ {
				r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
				require.Error(t, err)
				require.Nil(t, r.Prepared)
				if fault == "absent" {
					require.Equal(t, OutcomeUnknown, r.Outcome)
				} else {
					require.Equal(t, OutcomeCommitted, r.Outcome)
				}
			}
			require.Equal(t, 1, *writes)
			require.Equal(t, 1, a.signCalls)
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskArchiveCommandNativeCallbackFences(t *testing.T) {
	for _, fault := range []string{"certificate-nil", "certificate-foreign", "certificate-otheridentity", "certificate-error", "signature", "context", "window", "session", "sign-error", "sign-wire-error", "sign-claim-loss", "sign-finalsync-ABA", "sign-restore", "sign-operation", "sign-cancel", "sign-deadline", "clock-deadline"} {
		t.Run(fault, func(t *testing.T) {
			f, a, prepared, target := archiveNativeFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			if fault == "sign-deadline" || fault == "clock-deadline" {
				prepared.deadline = time.Now().Add(1200 * time.Millisecond)
			}
			if fault == "certificate-nil" {
				a.certificate = func(context.Context) ([]byte, error) { return nil, nil }
			}
			if fault == "certificate-foreign" {
				a.certificate = func(context.Context) ([]byte, error) { return []byte(`{}`), nil }
			}
			if fault == "certificate-error" {
				a.certificate = func(context.Context) ([]byte, error) {
					return bytes.Clone(a.old.wire), errors.New("certificate callback refused")
				}
			}
			if fault == "certificate-otheridentity" {
				var cert taskArchiveCertificateWire
				require.NoError(t, json.Unmarshal(a.old.wire, &cert))
				cert.Claims.CertificateID = uuid.NewString()
				wire, err := p.SignCommandIssuerCertificate(ed25519.NewKeyFromSeed(make([]byte, 32)), cert.Claims)
				require.NoError(t, err)
				a.certificate = func(context.Context) ([]byte, error) { return wire, nil }
			}
			a.sign = func(call context.Context, digest string, claims p.TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
				require.Equal(t, a.old.identity.Digest(), digest)
				switch fault {
				case "context":
					claims.Context.PlanDigest = strings.Repeat("a", 64)
				case "window":
					claims.NotAfter = claims.NotAfter.Add(-time.Second)
				case "session":
					claims.SessionNotAfter = claims.SessionNotAfter.Add(-time.Second)
				case "sign-error":
					return nil, errors.New("original signer refused")
				case "sign-claim-loss":
					_, err := f.raw.Delete(f.ctx, f.c.guardKey)
					require.NoError(t, err)
				case "sign-finalsync-ABA":
					key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
					require.NoError(t, err)
					_, err = f.raw.Delete(f.ctx, key)
					require.NoError(t, err)
					_, err = f.raw.Put(f.ctx, key, f.c.finalSyncDraft.value)
					require.NoError(t, err)
				case "sign-restore":
					_, err := f.raw.Put(f.ctx, f.b.restoreKey, "archive-restored")
					require.NoError(t, err)
				case "sign-operation":
					prefix, err := f.b.taskOperationsPrefix(f.c.reference.Task)
					require.NoError(t, err)
					_, err = f.raw.Put(f.ctx, prefix+uuid.NewString(), `{}`)
					require.NoError(t, err)
				case "sign-cancel":
					cancel()
				case "sign-deadline":
					<-call.Done()
					return nil, call.Err()
				}
				wire, err := p.SignTaskWorkspaceArchiveTicket(a.old.key, a.old.identity, claims)
				if fault == "sign-wire-error" {
					return wire, errors.New("signer callback refused")
				}
				if fault == "signature" && len(wire) > 0 {
					wire[len(wire)-5] ^= 1
				}
				return wire, err
			}
			if fault == "clock-deadline" {
				previousClock := f.b.authorityClock
				f.b.authorityClock = testAuthorityClock(func(call context.Context) (ClockObservation, error) {
					if f.c.finalSyncDraft.archive.claims.Version != 0 {
						<-call.Done()
						return ClockObservation{}, call.Err()
					}
					return previousClock.Observe(call)
				})
			}
			writes := archiveNativeCapture(t, f, nil, nil)
			r, err := f.b.PrepareTaskWorkspaceArchive(ctx, f.c, prepared, target.destination, 45*time.Second)
			require.Error(t, err)
			if fault == "certificate-otheridentity" {
				require.ErrorIs(t, err, tr.ErrDestination)
				require.Zero(t, a.signCalls)
			}
			require.Nil(t, r.Prepared)
			require.Equal(t, 0, *writes)
			d := f.c.finalSyncDraft.archive
			if fault == "certificate-nil" || fault == "certificate-foreign" || fault == "certificate-otheridentity" || fault == "certificate-error" {
				a.certificate = nil
				_, err = f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
				require.Error(t, err)
				require.Equal(t, 1, a.certCalls, "first certificate is frozen even nil")
			}
			if fault == "signature" || fault == "context" || fault == "window" || fault == "session" || fault == "sign-error" || fault == "sign-wire-error" {
				originalClaims, originalWire := d.claims, bytes.Clone(d.signedWire)
				a.sign = nil
				_, err = f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
				require.Error(t, err)
				require.Equal(t, 1, a.signCalls)
				require.Equal(t, originalClaims, d.claims)
				require.Equal(t, originalWire, d.signedWire)
			}
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskArchiveCommandNativeOriginalRefusals(t *testing.T) {
	f, a, prepared, target := archiveNativeFixture(t)
	writes := archiveNativeCapture(t, f, nil, nil)
	for _, duration := range []time.Duration{0, 30 * time.Second, time.Hour + time.Nanosecond} {
		r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, duration)
		require.Error(t, err)
		require.Nil(t, r.Prepared)
	}
	saved := f.c.finalSyncDraft
	f.c.finalSyncDraft = nil
	r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, r.Prepared)
	f.c.finalSyncDraft = saved
	require.Zero(t, a.signCalls)
	require.Zero(t, a.certCalls)
	require.Zero(t, *writes)
	r, err = f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, time.Hour)
	require.ErrorIs(t, err, ErrGuardExpired, "certificate must cover entire fixed session")
	require.Nil(t, r.Prepared)
	require.Zero(t, a.signCalls)
	certCalls := a.certCalls
	// Renewal cannot revive this original prerequisite's fixed deadline.
	prepared.deadline = time.Now().Add(-time.Millisecond)
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	r, err = f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, time.Hour)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.Nil(t, r.Prepared)
	require.Equal(t, certCalls, a.certCalls, "expired original prerequisite cannot invoke another callback")
}

func TestTaskArchiveCommandNativeCurrentClaimHistoricalQuiescence(t *testing.T) {
	f, a, prepared, target := archiveNativeFixture(t)
	old := prepared.receipt.Context()
	// A new original claimant can produce a NEW local prerequisite from terminal
	// old q; it cannot borrow the old final-sync producer. Remove the reserved
	// metadata here as an operator fixture before the new original reservation.
	key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
	require.NoError(t, err)
	_, err = f.raw.Delete(f.ctx, key)
	require.NoError(t, err)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
	c, err := f.b.ClaimTask(f.ctx, f.c.reference.Task, "archive-next-worker", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { taskReleaseClaim(t, f.b, c) })
	f.c = c
	next, err := f.b.PrepareAfterDualDrain(f.ctx, c, target.destination)
	require.NoError(t, err)
	reserved, err := f.b.ReserveTaskFinalSync(f.ctx, c, next, target.destination, finalSyncPlan())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, reserved.Outcome)
	r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, c, next, target.destination, 45*time.Second)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	d := c.finalSyncDraft.archive
	require.Equal(t, old, d.claims.Context.QuiescenceContext)
	require.Greater(t, d.claims.Context.Current.ClaimCreateRevision, old.Current.ClaimCreateRevision)
	require.Equal(t, c.reference.ClaimID, d.claims.Context.Current.ClaimID)
	require.Equal(t, c.reference.LeaseID, d.claims.Context.Current.LeaseID)
	require.Equal(t, "archive-next-worker", d.claims.Context.Current.WorkerID)
	require.Equal(t, 1, a.signCalls)
}

func TestTaskArchiveCommandNativeCommittedClaimAndABALoss(t *testing.T) {
	for _, fault := range []string{"claim", "command-ABA", "issuer-ABA", "finalsync-ABA", "quiescence-ABA", "quiescence-receipt-ABA", "quiescence-reservation-ABA", "restore"} {
		t.Run(fault, func(t *testing.T) {
			f, a, prepared, target := archiveNativeFixture(t)
			writes := archiveNativeCapture(t, f, nil, nil)
			r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
			require.NoError(t, err)
			require.NotNil(t, r.Prepared)
			d := f.c.finalSyncDraft.archive
			key, value := "", ""
			switch fault {
			case "claim":
				key = f.c.guardKey
			case "command-ABA":
				key, err = f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
				value = d.value
			case "issuer-ABA":
				key, value = d.issuerKey, d.issuerValue
			case "finalsync-ABA":
				key, err = f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
				value = d.finalSync.value
			case "quiescence-ABA":
				key, value = prepared.pin.intent.key, prepared.pin.intent.value
			case "quiescence-receipt-ABA":
				key, value = prepared.pin.receipt.key, prepared.pin.receipt.value
			case "quiescence-reservation-ABA":
				key = prepared.pin.reservation.key
				native, err := f.raw.Get(f.ctx, key)
				require.NoError(t, err)
				require.Len(t, native.Kvs, 1)
				value = string(native.Kvs[0].Value)
			case "restore":
				key, value = f.b.restoreKey, "archive-restored"
			}
			require.NoError(t, err)
			_, err = f.raw.Delete(f.ctx, key)
			require.NoError(t, err)
			if value != "" {
				_, err = f.raw.Put(f.ctx, key, value)
				require.NoError(t, err)
			}
			again, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
			require.Error(t, err)
			require.Nil(t, again.Prepared)
			if fault == "claim" || fault == "issuer-ABA" || fault == "finalsync-ABA" {
				require.Equal(t, OutcomeCommitted, again.Outcome)
			}
			require.Equal(t, 1, *writes)
			require.Equal(t, 1, a.signCalls)
		})
	}
}

func TestTaskArchiveCommandNativeConcurrentOriginal(t *testing.T) {
	f, a, prepared, target := archiveNativeFixture(t)
	writes := archiveNativeCapture(t, f, nil, nil)
	type result struct {
		preparation TaskWorkspaceArchivePreparationResult
		err         error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := f.b.PrepareTaskWorkspaceArchive(f.ctx, f.c, prepared, target.destination, 45*time.Second)
			results <- result{r, err}
		}()
	}
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotNil(t, first.preparation.Prepared)
	require.Same(t, first.preparation.Prepared, second.preparation.Prepared)
	require.Equal(t, first.preparation.Reference, second.preparation.Reference)
	require.Equal(t, 1, *writes)
	require.Equal(t, 1, a.certCalls)
	require.Equal(t, 1, a.signCalls)
}
