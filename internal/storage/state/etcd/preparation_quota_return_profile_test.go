package etcd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The original v1 codec fixture is sufficient to check branch selection and
// frozen-byte refusal without constructing a worker, client or signed detached
// positive. Detached objects below are deliberately incomplete refusal models.
func TestColdQuotaReturnProfileAndFrozenRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		validBranch bool
		validFrozen bool
		change      func(*coldQuotaReturnHistory, *coldQuotaReturnPrepared)
	}{
		{
			name: "original-v1", validBranch: true, validFrozen: true,
			change: func(_ *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {},
		},
		{
			name: "empty",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				h.release = nil
			},
		},
		{
			name: "mixed",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				h.profileRelease = &taskOwnerReleaseProof{detached: &detachedOwnerReleaseProof{}}
			},
		},
		{
			name: "union-local-is-not-legacy",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				h.profileRelease = &taskOwnerReleaseProof{local: h.release}
				h.release = nil
			},
		},
		{
			name: "mixed-union",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				h.profileRelease = &taskOwnerReleaseProof{local: h.release, detached: &detachedOwnerReleaseProof{}}
				h.release = nil
			},
		},
		{
			name: "unproven-detached",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				h.release = nil
				h.profileRelease = &taskOwnerReleaseProof{detached: &detachedOwnerReleaseProof{}}
			},
		},
		{
			name: "foreign-v2-refusal-model",
			change: func(h *coldQuotaReturnHistory, _ *coldQuotaReturnPrepared) {
				model := detachedQuotaReturnCodecModel(t)
				model.Release.Task.SandboxID = "foreign-sandbox"
				h.profileRelease = &taskOwnerReleaseProof{detached: &detachedOwnerReleaseProof{record: model.Release, kv: h.release.kv}}
				h.release = nil
			},
		},
		{
			name: "changed-frozen-bytes", validBranch: true,
			change: func(_ *coldQuotaReturnHistory, prepared *coldQuotaReturnPrepared) {
				prepared.mutation.Writes[0].Value[0] ^= 1
			},
		},
		{
			name: "changed-frozen-locator", validBranch: true,
			change: func(_ *coldQuotaReturnHistory, prepared *coldQuotaReturnPrepared) {
				prepared.reference.RequestID += "-foreign"
			},
		},
		{
			name: "missing-frozen-return", validBranch: true,
			change: func(_ *coldQuotaReturnHistory, prepared *coldQuotaReturnPrepared) {
				prepared.mutation.Writes = nil
			},
		},
		{
			name: "duplicate-frozen-return", validBranch: true,
			change: func(_ *coldQuotaReturnHistory, prepared *coldQuotaReturnPrepared) {
				prepared.mutation.Writes = append(prepared.mutation.Writes, prepared.mutation.Writes[0])
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, kv, _, record, ref := coldQuotaProofFixture(t)
			wire, err := b.coldQuotaReturnWire(h, record.Attempt)
			require.NoError(t, err)
			require.Equal(t, string(kv.Value), wire, "legacy bytes remain identical")
			prepared := &coldQuotaReturnPrepared{
				history: h, reference: ref, returnKey: "original-return-key",
				mutation: Mutation{Writes: []Write{{Key: "original-return-key", Value: []byte(wire)}}},
			}
			tc.change(h, prepared)

			binding, branchErr := coldQuotaReturnRelease(h)
			if tc.validBranch {
				require.NoError(t, branchErr)
				require.Same(t, h.release, binding.local)
				require.Nil(t, binding.detached)
			} else {
				require.ErrorIs(t, branchErr, ErrInvalidRecord)
				_, err := b.coldQuotaReturnWire(h, record.Attempt)
				require.ErrorIs(t, err, ErrInvalidRecord)
				_, err = b.decodeColdQuotaReturnProof(h, kv)
				require.ErrorIs(t, err, ErrCorruptRecord)
			}

			frozenErr := b.validateColdQuotaReturnFrozenValue(prepared)
			if tc.validFrozen {
				require.NoError(t, frozenErr)
			} else {
				require.ErrorIs(t, frozenErr, ErrInvalidMutation)
			}
		})
	}
}
