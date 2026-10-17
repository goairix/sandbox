package etcd

import (
	"crypto/ed25519"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These are actual original protocol signatures and clock-window checks, not
// a fabricated TaskClaim or physical effect authority. The caller supplies the
// minimum of its real claim/context lifetimes to this shared pure constructor.
func TestLocalRuntimeTerminationClockWindow(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                                 string
		remaining, issuerBefore, issuerAfter time.Duration
		secondSample, wantBefore, wantAfter  time.Duration
		windowRefused, verificationRefused   bool
	}{
		{
			name:      "sample-regresses-100ms",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			secondSample: -100 * time.Millisecond, wantBefore: -2 * time.Second, wantAfter: 19 * time.Second,
		},
		{
			name:      "exact-lower-edge",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			secondSample: -time.Second, wantBefore: -2 * time.Second, wantAfter: 19 * time.Second,
		},
		{
			name:      "excess-regression-refused",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			secondSample: -time.Second - time.Nanosecond, wantBefore: -2 * time.Second, wantAfter: 19 * time.Second,
			verificationRefused: true,
		},
		{
			name:      "expiry-refused",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			secondSample: 18 * time.Second, wantBefore: -2 * time.Second, wantAfter: 19 * time.Second,
			verificationRefused: true,
		},
		{
			name:      "issuer-lower-clamp",
			remaining: 20 * time.Second, issuerBefore: -1500 * time.Millisecond, issuerAfter: time.Minute,
			wantBefore: -1500 * time.Millisecond, wantAfter: 19 * time.Second,
		},
		{
			name:      "issuer-lower-clamp-refused",
			remaining: 20 * time.Second, issuerBefore: -time.Second + time.Nanosecond, issuerAfter: time.Minute,
			windowRefused: true,
		},
		{
			name:      "issuer-upper-clamp",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: 10 * time.Second,
			wantBefore: -2 * time.Second, wantAfter: 9 * time.Second,
		},
		{
			name:      "issuer-expiry-refused",
			remaining: 20 * time.Second, issuerBefore: -time.Minute, issuerAfter: 3 * time.Second,
			windowRefused: true,
		},
		{
			name:      "remaining-cap-20s",
			remaining: time.Minute, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			wantBefore: -2 * time.Second, wantAfter: 19 * time.Second,
		},
		{
			name:      "claim-minimum-input",
			remaining: 7 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			wantBefore: -2 * time.Second, wantAfter: 6 * time.Second,
		},
		{
			name:      "context-minimum-input",
			remaining: 5 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			wantBefore: -2 * time.Second, wantAfter: 4 * time.Second,
		},
		{
			name:      "short-remaining",
			remaining: 3 * time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			windowRefused: true,
		},
		{
			name:      "expired-remaining",
			remaining: -time.Second, issuerBefore: -time.Minute, issuerAfter: time.Minute,
			windowRefused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := localReleaseTerminationFixture(t).Current
			root := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
			key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
			defer clear(root)
			defer clear(key)
			binding := p.TrustBinding{Namespace: current.Namespace, AuthorityID: current.AuthorityID, Target: current.Target, RestoreEpoch: current.RestoreEpoch}
			certificate, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{
				Version: 1, Role: "command_issuer", CertificateID: uuid.NewString(),
				Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch,
				PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(tc.issuerBefore), NotAfter: now.Add(tc.issuerAfter),
			})
			require.NoError(t, err)
			verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, err)
			// Obtain an authentic identity at the certificate's interior so
			// the pure constructor's lower-clamp refusal is independently
			// exercised. Actual x.sign also verifies it at its current sample.
			certificateMiddle := now.Add(tc.issuerBefore + (tc.issuerAfter-tc.issuerBefore)/2)
			issuer, err := verifier.VerifyCommandIssuerCertificate(certificate, certificateMiddle)
			require.NoError(t, err)

			before, after, err := localRuntimeTerminationWindow(now, tc.remaining, issuer)
			if tc.windowRefused {
				require.ErrorIs(t, err, ErrGuardExpired)
				return
			}
			require.NoError(t, err)
			require.Equal(t, now.Add(tc.wantBefore), before)
			require.Equal(t, now.Add(tc.wantAfter), after)
			require.LessOrEqual(t, after.Sub(before), 30*time.Second)
			require.False(t, before.Before(issuer.NotBefore()))
			require.True(t, after.Before(issuer.NotAfter()))
			require.False(t, now.Add(-time.Second).Before(before))
			require.True(t, now.Add(2*time.Second).Before(after))

			current.IssuerCertificateID, current.IssuerCertificateDigest = issuer.CertificateID(), issuer.Digest()
			hash, err := hex.DecodeString(current.WorkspaceHash)
			require.NoError(t, err)
			require.Len(t, hash, 32)
			claims := p.TaskRuntimeTerminationTicketClaims{
				Version: 1, Purpose: "task_runtime_terminate", Current: current, Partition: hash[0],
				IntentCreateRevision: current.ClaimCreateRevision + 1, IntentModRevision: current.ClaimCreateRevision + 1,
				IntentDigest: strings.Repeat("a", 64), EnrollmentDigest: strings.Repeat("b", 64), ObserverCertificateDigest: strings.Repeat("c", 64),
				NodeUID: "11111111-1111-4111-8111-111111111111", NodeBootID: "22222222-2222-4222-8222-222222222222",
				NotBefore: before, NotAfter: after,
			}
			wire, err := p.SignTaskRuntimeTerminationTicket(key, issuer, claims)
			require.NoError(t, err)
			_, err = verifier.VerifyTaskRuntimeTerminationTicket(wire, certificate, claims, now.Add(tc.secondSample))
			if tc.verificationRefused {
				require.ErrorContains(t, err, "outside authentication window")
			} else {
				require.NoError(t, err)
			}

			if tc.name == "sample-regresses-100ms" {
				// Preserve the exact observed defect independently: a window
				// beginning at now-1s has no margin for a second valid sample
				// whose elapsed-accounted estimate is slightly earlier.
				claims.NotBefore = now.Add(-time.Second)
				oldWire, err := p.SignTaskRuntimeTerminationTicket(key, issuer, claims)
				require.NoError(t, err)
				_, err = verifier.VerifyTaskRuntimeTerminationTicket(oldWire, certificate, claims, now.Add(tc.secondSample))
				require.ErrorContains(t, err, "outside authentication window")
			}
		})
	}
}
