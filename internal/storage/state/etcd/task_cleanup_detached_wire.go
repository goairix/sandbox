package etcd

import (
	"strings"
	"time"
)

// Detached wire inputs are dimensional copies, never native authority. The
// original source reader, registered job, Kernel and current E/S/Run still
// have to authenticate each managed inventory before it can reach Grant.
type cleanupDetachedWireInput struct {
	bundle      *taskClaimBundle
	snapshot    SandboxSnapshotRecord
	intent      CreationIntentRecord
	workspace   WorkspaceIdentity
	publication *RuntimeEnrolledPublicationEntry
	issuer      CommandIssuerEntry
	workerID    string
	claimTTL    time.Duration
}

// An emission borrows exactly one Txn or Lease projection for the duration of
// the visitor. The producer must return that frame before constructing another;
// neither an emission nor the compact report grants effect authority.
type cleanupDetachedWireEmission struct {
	family, variant string
	txn             *cleanupWireProjection
	lease           *cleanupLeaseWireProjection
}

type cleanupDetachedWireReport struct {
	complete                                           bool
	variants                                           int
	maxNodes, requestBytes, successBytes, failureBytes int
	retainedKeys                                       int
}

// Visit one complete closed manifest without retaining a protobuf plan. The
// production preparation caller remains behind its pre-E admission boundary;
// this compact report does not manufacture a live inventory or permission.
func (b *Backend) visitCleanupDetachedWire(in cleanupDetachedWireInput, dimensions cleanupLocalWireDimensions, emit func(cleanupDetachedWireEmission) error) (cleanupDetachedWireReport, error) {
	if emit == nil {
		return cleanupDetachedWireReport{}, ErrInvalidRecord
	}
	w, err := b.prepareDetachedWireBuilder(in, dimensions)
	if err != nil {
		return cleanupDetachedWireReport{}, err
	}
	for _, family := range cleanupDetachedWireManifest {
		for _, variant := range strings.Fields(family.variants) {
			if err := w.emitVariant(family.name, variant, emit); err != nil {
				return w.report, err
			}
		}
	}
	for step := 0; step < 7; step++ {
		for _, phase := range []string{"intent", "unknown", "complete", "guard", "skipped"} {
			if phase == "skipped" && (step < 2 || step > 5) {
				continue
			}
			if err := w.emitProviderStep(step, phase, emit); err != nil {
				return w.report, err
			}
		}
	}
	w.report.complete = true
	return w.report, nil
}

// Allocation dimensions must be checked before padding, client comparisons or
// protobuf objects exist. Size checks on the finished protobuf are additional.
type cleanupDetachedWireAllocation struct {
	keyBytes, valueBytes, schemaBytes uint64
	nodes, keys                       int
}

func checkCleanupDetachedWireAllocation(allocation cleanupDetachedWireAllocation) error {
	// Cap each operand before adding. The6 multiplier bounds three distinct
	// data backings at capacity<=2n; shallow Compare copies are not a fourth.
	if allocation.nodes < 1 || allocation.nodes > 64 || allocation.keys < 1 || allocation.keys > 64 || allocation.keyBytes > 64<<10 || allocation.valueBytes > 640<<10 || allocation.schemaBytes > 176128 {
		return ErrInvalidConfiguration
	}
	data := allocation.keyBytes + allocation.valueBytes
	if data > 640<<10 {
		return ErrInvalidConfiguration
	}
	owned := uint64(2<<20) + 6*data + 4*(6*65536+4096) + 2*allocation.schemaBytes + 128<<10
	if owned > 8<<20 {
		return ErrInvalidConfiguration
	}
	return nil
}
