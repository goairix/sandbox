package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func TestColdCreateDeploymentRootSigner(t *testing.T) {
	b, material, _, _, now := coldMaterialFixture(t)
	b.requestTimeout = time.Second
	b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
	var envelope struct {
		Claims    p.CommandIssuerCertificateClaims `json:"claims"`
		Signature []byte                           `json:"signature"`
	}
	require.NoError(t, json.Unmarshal(material.IssuerCertificate, &envelope))
	rootA := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	rootB := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	launcher, err := p.NewManagementVerifier(p.TrustBinding{Namespace: envelope.Claims.Namespace, AuthorityID: envelope.Claims.AuthorityID, Target: envelope.Claims.Target, RestoreEpoch: envelope.Claims.RestoreEpoch}, []ed25519.PublicKey{rootA.Public().(ed25519.PublicKey), rootB.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	_, err = launcher.VerifyCommandIssuerCertificate(material.IssuerCertificate, now)
	require.NoError(t, err, "launcher trusts A and B; existing issuer is signed by A")
	for _, tc := range []struct {
		key      ed25519.PrivateKey
		accepted bool
	}{{rootA, true}, {rootB, false}} {
		proof, err := p.SignCommandIssuerCertificate(tc.key, envelope.Claims)
		require.NoError(t, err)
		_, err = launcher.VerifyCommandIssuerCertificate(proof, now)
		require.NoError(t, err, "launcher membership alone admits both roots")
		err = b.CheckColdCreateRootSigner(context.Background(), proof)
		if tc.accepted {
			require.NoError(t, err)
		} else {
			require.Error(t, err, "Backend trusts only A; root B must fail before allocation/POST")
		}
	}
}

func TestColdCreateDeploymentDisabledQuota(t *testing.T) {
	b, material, _, _, _ := coldMaterialFixture(t)
	b.archiveLife = &taskArchiveBackend{}
	b.preparationInventory = &PreparationInventoryOptions{}
	b.preparationInventory.QuotaSlots[7] = 1
	b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		t.Fatal("disabled deployment quota must fail before clock or native RPC")
		return ClockObservation{}, nil
	})
	_, err := b.CheckColdCreateDeployment(context.Background(), p.TrustBinding{}, "", material.IssuerCertificate, 8)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}
