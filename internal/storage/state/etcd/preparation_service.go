package etcd

import (
	"context"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// CheckColdCreateDeployment checks one fixed service profile against the
// original Backend's configured trust and durable ready barrier. It grants no
// native permission and performs no inventory scan or initialization.
func (b *Backend) CheckColdCreateDeployment(ctx context.Context, binding p.TrustBinding, contract string, issuer []byte, quota uint8) (string, error) {
	if b == nil || b.enrolledPublication == nil || b.enrolledVerifier == nil || b.archiveLife == nil || len(issuer) == 0 || len(issuer) > 8192 {
		return "", ErrInvalidConfiguration
	}
	if b.preparationInventory == nil || b.preparationInventory.QuotaSlots[quota] == 0 {
		return "", ErrInvalidConfiguration
	}
	b.archiveLife.mu.Lock()
	closed := b.archiveLife.closed
	b.archiveLife.mu.Unlock()
	if closed {
		return "", ErrGuardExpired
	}
	if binding != (p.TrustBinding{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: b.restoreEpoch}) || contract != b.enrolledPublication.ContractDigest {
		return "", ErrInvalidConfiguration
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return "", err
	}
	if _, err := b.enrolledVerifier.VerifyCommandIssuerCertificate(issuer, now); err != nil {
		return "", err
	}
	if _, _, err := b.coldReady(ctx); err != nil {
		return "", err
	}
	return b.preparationDigest, nil
}

// CheckColdCreateRootSigner is a constructor-only verification of the explicit
// signer's public probe with the original Backend roots. It persists nothing.
func (b *Backend) CheckColdCreateRootSigner(ctx context.Context, proof []byte) error {
	if b == nil || b.enrolledVerifier == nil || len(proof) == 0 || len(proof) > 8192 {
		return ErrInvalidConfiguration
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	_, err = b.enrolledVerifier.VerifyCommandIssuerCertificate(proof, now)
	return err
}
