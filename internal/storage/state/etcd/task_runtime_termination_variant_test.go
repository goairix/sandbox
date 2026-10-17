package etcd

import (
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

// Matching only the comparable common stamps loses the variant discriminator.
// Even a retained reservation must match the entire original effect profile.
func TestRuntimeTerminationMatcherRejectsForeignDetachedVariant(t *testing.T) {
	r := localReleaseTerminationFixture(t)
	a := &taskLocalRuntimeAuthority{
		claim:  &TaskClaim{birth: r.TaskBirth, reference: TaskClaimReference{CreateRevision: r.Current.ClaimCreateRevision}},
		task:   TaskRecord{Reference: TaskReference{Partition: r.Partition}, CreationIntentID: r.CreationIntentID},
		stamps: r.Authority,
		publication: &RuntimeEnrolledPublicationEntry{Record: RuntimeEnrolledPublicationRecord{
			EnrollmentDigest: r.EnrollmentDigest, ObserverCertificateDigest: r.ObserverCertificateDigest,
			NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
		}},
	}
	a.command.Claims.Context.Current = r.Current
	require.True(t, a.matchesTermination(r), "fixture retains the actual local origin")

	foreign := r
	foreign.Version = 2
	foreign.DetachedEffects = &p.DetachedRuntimeEffectBindings{
		Version: 1, Profile: "detached_enrolled_plain", ContractDigest: strings.Repeat("a", 64),
		Settlement: p.RecordStamp{CreateRevision: 1, ModRevision: 1, SHA256: strings.Repeat("b", 64)},
	}
	require.False(t, a.matchesTermination(foreign), "common stamps cannot turn local authority into detached authority")

	foreign.Version = 1
	require.False(t, a.matchesTermination(foreign), "local origin must also reject an extra detached tuple")
}
