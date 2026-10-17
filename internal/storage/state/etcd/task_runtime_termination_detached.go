package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Only the original registered continuation can enter this detached profile.
// The permanent fact and current Claim/Run are independently authenticated by
// the loader. Admission, cold post-release history and owner release remain
// separate entry points; a copied wire tuple supplies none of these capabilities.
func (b *Backend) terminateTaskRuntimeDetached(ctx context.Context, j *cleanupLocalJob, c *TaskClaim, destination *controltransport.TerminationDestination, reservation *taskFence) (result TaskRuntimeTerminationResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || destination == nil {
		return result, ErrInvalidRecord
	}
	if err := b.validateDetachedRuntimeOwner(j, c); err != nil {
		return result, err
	}
	c.mu.Lock()
	repeated := c.runtimeReleaseAttempt != nil
	c.mu.Unlock()
	if repeated {
		if reservation != nil {
			return result, ErrInvalidRecord
		}
		return b.diagnoseDetachedRuntimeTermination(ctx, j, c)
	}

	o, err := b.beginLocalRuntimeRelease(ctx, c)
	if err != nil {
		return result, err
	}
	defer o.finish()
	a, err := b.loadDetachedRuntimeAuthority(o.ctx, j, c)
	if err != nil {
		return result, err
	}
	// Renewal may extend the Claim, but never this accepted operation's
	// captured window. Propagate that cutoff into every dependent callback,
	// provider/native RPC and observer send, rather than only checking locally.
	execution, cancel := context.WithDeadline(o.ctx, a.detached.cutoff)
	defer cancel()
	x, err := b.loadRuntimeTerminationExecution(execution, a, reservation)
	if err != nil {
		return result, err
	}
	return b.executeRuntimeTermination(execution, x, destination)
}

// Same-Claim diagnosis borrows the original bounded read slot and joins it on
// Close. It independently reauthenticates the v2 live history, then returns only
// persisted metadata. It allocates no candidate/command and never enters the
// provider, issuer or observer, even when the earlier reservation ACK was lost.
func (b *Backend) diagnoseDetachedRuntimeTermination(ctx context.Context, j *cleanupLocalJob, c *TaskClaim) (result TaskRuntimeTerminationResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	o, err := b.beginLocalRuntimeRead(ctx, c.reference.Task)
	if err != nil {
		return result, err
	}
	defer o.finish()
	a, err := b.loadDetachedRuntimeAuthority(o.ctx, j, c)
	if err != nil {
		return result, err
	}
	// An independent read captures its own current legal window. Its later
	// pair RPC inherits that cutoff even if this same Claim renews again.
	diagnosis, cancel := context.WithDeadline(o.ctx, a.detached.cutoff)
	defer cancel()
	x, err := b.loadRuntimeTerminationExecution(diagnosis, a, nil)
	if err != nil || x.intentKV == nil {
		return result, err
	}
	result.Entry = localRuntimeTerminationEntry(x.intent, x.intentKV)
	if x.intent.Phase == "cleanup-complete" {
		result.Outcome = OutcomeCommitted
	}
	return result, nil
}

// Profile selection uses only the original loader's private authenticated pins.
// Neither an intent DTO nor a configured unrelated issuer selects a certificate.
func (a *taskLocalRuntimeAuthority) issuerCertificate() []byte {
	if a.detached != nil {
		return a.detached.certificate
	}
	return a.command.Certificate
}

func (a *taskLocalRuntimeAuthority) contractDigest() string {
	if a.detached != nil {
		return a.detached.effects.ContractDigest
	}
	return a.backend.enrolledPublication.ContractDigest
}

// The original v1 wrapper stays strict. V2 additionally requires this genuine
// loader's whole variant before accepting the actual create-once reservation;
// absent, recreated or changed native intent cannot acquire a new send grant.
func (a *taskLocalRuntimeAuthority) matchTerminationReservation(expected *taskFence, actual *mvccpb.KeyValue) error {
	if a.detached == nil {
		return matchCleanupTerminationReservation(a.task.Reference, a.claim.birth, expected, actual)
	}
	if expected == nil || actual == nil || expected.lease != 0 || expected.create <= 0 || expected.mod != expected.create || actual.Lease != 0 || actual.Version != 1 || actual.CreateRevision != actual.ModRevision || *expected != ownTaskFence(actual) {
		return ErrConflict
	}
	intent, err := decodeLocalRuntimeTermination(actual)
	if err != nil || intent.Phase != "reserved" || !a.matchesTermination(intent) {
		return errors.Join(ErrCorruptRecord, err)
	}
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(intent)
	if err != nil || keys.Intent != expected.key {
		return errors.Join(ErrCorruptRecord, err)
	}
	return nil
}
