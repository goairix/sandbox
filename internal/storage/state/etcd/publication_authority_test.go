package etcd

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type testAuthorityClock func(context.Context) (ClockObservation, error)

func (f testAuthorityClock) Observe(ctx context.Context) (ClockObservation, error) { return f(ctx) }

type nilChannelAuthorityClock chan struct{}

func (nilChannelAuthorityClock) Observe(context.Context) (ClockObservation, error) {
	panic("configuration observed nil channel clock")
}

func TestPublicationRejectsNilChannelAuthorityClock(t *testing.T) {
	o := validOptions(t)
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
	o.Clock = nilChannelAuthorityClock(nil)
	v, err := publicationVerifier(o)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, v)
	require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
	b, err := New(context.Background(), o)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, b)
}

func goodAuthorityClock() AuthorityClock {
	return testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: time.Now().UTC()}, nil })
}
func TestPublicationTrustConfiguration(t *testing.T) {
	for _, fault := range []string{"trust only", "clock only", "authority", "target", "root"} {
		t.Run(fault, func(t *testing.T) {
			o := validOptions(t)
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			o.Clock = goodAuthorityClock()
			switch fault {
			case "trust only":
				o.Clock = nil
			case "clock only":
				o.PublicationTrust = nil
			case "authority":
				o.PublicationTrust.AuthorityID = "other"
			case "target":
				o.PublicationTrust.Target = ""
			case "root":
				o.PublicationTrust.Roots[0] = make([]byte, 31)
			}
			require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration, "invalid pinned dependencies must be rejected before dial")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err := New(ctx, o)
			require.ErrorIs(t, err, ErrInvalidConfiguration)
		})
	}
}
func TestPublicationClockFailClosed(t *testing.T) {
	b, _ := integrationBackend(t)
	_, err := b.observePublicationClock(context.Background())
	require.Error(t, err, "metadata-only backend cannot substitute system time")
}

var errTestClock = errors.New("clock unavailable")

func TestPublicationControlledClock(t *testing.T) {
	for _, fault := range []string{"valid", "unknown", "negative", "overflow", "budget elapsed", "non UTC", "error", "canceled"} {
		t.Run(fault, func(t *testing.T) {
			base, raw := integrationBackend(t)
			o := integrationOptions(base, raw)
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			o.Clock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
				result := ClockObservation{UTC: now}
				switch fault {
				case "unknown":
					result.UTC = time.Time{}
				case "negative":
					result.Uncertainty = -1
				case "overflow":
					result.Uncertainty = time.Duration(1<<63 - 1)
				case "budget elapsed":
					result.Uncertainty = time.Second
				case "non UTC":
					result.UTC = now.In(time.FixedZone("offset", 0))
				case "error":
					return result, errTestClock
				}
				return result, nil
			})
			b, err := New(context.Background(), o)
			require.NoError(t, err)
			defer b.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fault == "canceled" {
				cancel()
			}
			actual, err := b.observePublicationClock(ctx)
			if fault == "valid" {
				require.NoError(t, err)
				require.False(t, actual.Before(now))
				require.True(t, actual.Before(now.Add(time.Second)))
			} else {
				require.Error(t, err)
				require.True(t, actual.IsZero())
			}
		})
	}
}

func TestPublicationConstructorCopiesRoots(t *testing.T) {
	base, raw := integrationBackend(t)
	o := integrationOptions(base, raw)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	root := append(ed25519.PublicKey(nil), key.Public().(ed25519.PublicKey)...)
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root}}
	o.Clock = goodAuthorityClock()
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	defer b.Close()
	now := time.Now().UTC()
	context := controlprotocol.CertificateContext{Namespace: b.namespace.Root(), AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", RestoreEpoch: b.restoreEpoch, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: domainHash, Generation: 1, OperationID: "11234567-89ab-4cde-8012-3456789abcde", PayloadDigest: domainHash, Snapshot: controlprotocol.SnapshotReference{Version: "v1", Digest: domainHash}, ExpiresAt: now.Add(time.Hour)}
	claims := controlprotocol.RuntimeCertificateClaims{Version: 1, Namespace: context.Namespace, AuthorityID: context.AuthorityID, Target: context.Target, RestoreEpoch: context.RestoreEpoch, IntentID: context.IntentID, SandboxID: context.SandboxID, WorkspaceHash: context.WorkspaceHash, Generation: 1, OperationID: context.OperationID, PayloadDigest: context.PayloadDigest, Snapshot: context.Snapshot, ExpiresAt: context.ExpiresAt, Runtime: controlprotocol.RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, WorkspaceMode: "plain", PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)}
	wire, err := controlprotocol.SignRuntimeCertificate(key, claims)
	require.NoError(t, err)
	for i := range root {
		root[i] ^= 0xff
	}
	o.PublicationTrust.AuthorityID = "other"
	o.PublicationTrust.Target = "other"
	evidence, err := b.publicationVerifier.VerifyCertificate(wire, context, now)
	require.NoError(t, err)
	require.Equal(t, "uid", evidence.Runtime().UID)
	require.Equal(t, context.AuthorityID, b.publicationAuthorityID)
	require.Equal(t, "kubernetes", b.publicationTarget)
}
