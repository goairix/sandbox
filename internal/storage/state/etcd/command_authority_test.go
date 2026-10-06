package etcd

import (
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

type unusedExecIssuer struct{}

func (*unusedExecIssuer) Certificate(context.Context) ([]byte, error) {
	panic("constructor called Certificate")
}
func (*unusedExecIssuer) SignStart(context.Context, string, controlprotocol.ExecStartTicketClaims) ([]byte, error) {
	panic("constructor called SignStart")
}

func TestExecCommandAuthorityConfiguration(t *testing.T) {
	t.Run("optional metadata only", func(t *testing.T) {
		o := validOptions(t)
		v, err := execAuthority(o)
		require.NoError(t, err)
		require.Nil(t, v)
		require.NoError(t, validateOptions(o))
	})
	for _, fault := range []string{"typed nil", "no trust", "no trust or clock", "no clock", "typed nil clock", "nil channel clock", "wrong authority", "empty roots", "three roots", "short root", "bad target", "bad epoch", "bad namespace"} {
		t.Run(fault, func(t *testing.T) {
			o := validOptions(t)
			o.ExecIssuer = &unusedExecIssuer{}
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, 32)}}
			o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("constructor observed clock") })
			switch fault {
			case "typed nil":
				o.ExecIssuer = (*unusedExecIssuer)(nil)
			case "no trust":
				o.PublicationTrust = nil
			case "no trust or clock":
				o.PublicationTrust = nil
				o.Clock = nil
			case "no clock":
				o.Clock = nil
			case "typed nil clock":
				o.Clock = testAuthorityClock(nil)
			case "nil channel clock":
				o.Clock = nilChannelAuthorityClock(nil)
			case "wrong authority":
				o.PublicationTrust.AuthorityID = "other"
			case "empty roots":
				o.PublicationTrust.Roots = nil
			case "three roots":
				o.PublicationTrust.Roots = []ed25519.PublicKey{make([]byte, 32), make([]byte, 32), make([]byte, 32)}
			case "short root":
				o.PublicationTrust.Roots[0] = make([]byte, 31)
			case "bad target":
				o.PublicationTrust.Target = ""
			case "bad epoch":
				o.Identity.RestoreEpoch = "../epoch"
			case "bad namespace":
				o.Namespace = Namespace{}
			}
			_, err := execAuthority(o)
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.ErrorIs(t, validateOptions(o), ErrInvalidConfiguration)
			b, err := New(context.Background(), o)
			require.Nil(t, b)
			require.ErrorIs(t, err, ErrInvalidConfiguration)
		})
	}
	t.Run("copied roots and exact binding", func(t *testing.T) {
		o := validOptions(t)
		o.ExecIssuer = &unusedExecIssuer{}
		rootKey := ed25519.NewKeyFromSeed(make([]byte, 32))
		delegate := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
		root := append(ed25519.PublicKey(nil), rootKey.Public().(ed25519.PublicKey)...)
		o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root, ed25519.NewKeyFromSeed([]byte("23456789012345678901234567890123")).Public().(ed25519.PublicKey)}}
		o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("constructor observed clock") })
		require.NoError(t, validateOptions(o))
		v, err := execAuthority(o)
		require.NoError(t, err)
		require.NotNil(t, v)
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
		claims := controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: "11234567-89ab-4cde-8012-3456789abcde", Role: "command_issuer", Namespace: o.Namespace.Root(), AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", RestoreEpoch: o.Identity.RestoreEpoch, PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)}
		wire, err := controlprotocol.SignCommandIssuerCertificate(rootKey, claims)
		require.NoError(t, err)
		for i := range root {
			root[i] ^= 0xff
		}
		o.PublicationTrust.Roots = nil
		o.PublicationTrust.AuthorityID, o.PublicationTrust.Target = "other", "other"
		o.Namespace = Namespace{}
		o.Identity.RestoreEpoch = "other"
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := v.VerifyCommandIssuerCertificate(wire, now)
				require.NoError(t, err)
			}()
		}
		wg.Wait()
		for _, change := range []func(*controlprotocol.CommandIssuerCertificateClaims){
			func(c *controlprotocol.CommandIssuerCertificateClaims) { c.Namespace = "/test/authority/other/" },
			func(c *controlprotocol.CommandIssuerCertificateClaims) { c.AuthorityID = "other" },
			func(c *controlprotocol.CommandIssuerCertificateClaims) { c.Target = "other" },
			func(c *controlprotocol.CommandIssuerCertificateClaims) { c.RestoreEpoch = "other" },
		} {
			changed := claims
			change(&changed)
			wire, err := controlprotocol.SignCommandIssuerCertificate(rootKey, changed)
			require.NoError(t, err)
			_, err = v.VerifyCommandIssuerCertificate(wire, now)
			require.Error(t, err)
		}
	})
}
