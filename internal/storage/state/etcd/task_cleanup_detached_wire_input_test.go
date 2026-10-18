package etcd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the real builder entry, not its scalar budget helper. In particular
// an oversized fence must be rejected before taskClaimWireComparisons copies
// it, even though normal managed readers already cap that native point.
func TestCleanupDetachedWireInputRejectedBeforeCopies(t *testing.T) {
	b, original, dimensions := cleanupDetachedWireFixture(t)
	large := strings.Repeat("x", 8<<20)
	wire := bytes.Repeat([]byte{'x'}, 8<<20)
	for name, change := range map[string]func(*cleanupDetachedWireInput){
		"fence-value":           func(in *cleanupDetachedWireInput) { in.bundle.fences[0].value = large },
		"fence-key":             func(in *cleanupDetachedWireInput) { in.bundle.fences[0].key = large },
		"checkpoint-value":      func(in *cleanupDetachedWireInput) { in.bundle.checkpoint.Value = wire },
		"checkpoint-key":        func(in *cleanupDetachedWireInput) { in.bundle.checkpoint.Key = wire },
		"issuer-certificate":    func(in *cleanupDetachedWireInput) { in.issuer.Record.Certificate = wire },
		"snapshot-payload":      func(in *cleanupDetachedWireInput) { in.snapshot.Payload = wire },
		"publication-proof":     func(in *cleanupDetachedWireInput) { in.publication.Proof = wire },
		"enrollment-receipt":    func(in *cleanupDetachedWireInput) { in.publication.Enrollment.Receipt = wire },
		"publication-namespace": func(in *cleanupDetachedWireInput) { in.publication.Record.Attempt.Namespace = large },
	} {
		t.Run(name, func(t *testing.T) {
			in := original
			bundle, checkpoint, publication := *in.bundle, *in.bundle.checkpoint, *in.publication
			bundle.fences = append([]taskFence(nil), in.bundle.fences...)
			bundle.checkpoint = &checkpoint
			in.bundle, in.publication = &bundle, &publication
			change(&in)
			visits := 0
			allocations := testing.AllocsPerRun(10, func() {
				report, err := b.visitCleanupDetachedWire(in, dimensions, func(cleanupDetachedWireEmission) error {
					visits++
					return nil
				})
				if err == nil || report.complete {
					t.Fatal("oversized actual input reached projection materialization")
				}
			})
			require.Zero(t, visits)
			require.LessOrEqual(t, allocations, float64(32), "bounded scalar validation must precede pb/comparison construction")
		})
	}
}
