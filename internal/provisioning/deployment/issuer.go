package deployment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"reflect"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
)

// ExecIssuer owns one mounted deployment's signing key. Backend borrows its
// public signing interface; the owner closes it after Service and Backend join.
type ExecIssuer struct {
	self        *ExecIssuer
	mu          sync.RWMutex
	closed      bool
	key         ed25519.PrivateKey
	certificate []byte
	identity    p.CommandIssuerIdentity
	binding     p.TrustBinding
}

// NewExecIssuer validates and copies mounted material without registering any
// metadata. Actual PrepareExecEffect retains the existing lazy registration.
func NewExecIssuer(ctx context.Context, key ed25519.PrivateKey, certificate []byte, config launcherconfig.Config, clock p.AuthorityClock) (*ExecIssuer, error) {
	if ctx == nil || clock == nil || len(key) != ed25519.PrivateKeySize {
		return nil, errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v := reflect.ValueOf(clock)
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Func || v.Kind() == reflect.Interface) && v.IsNil() {
		return nil, errConfiguration
	}
	private := bytes.Clone(key)
	installed := false
	defer func() {
		if !installed {
			clear(private)
		}
	}()
	wire := bytes.Clone(certificate)
	credential, err := p.NewManagementTLSCredential(private, wire, "command_issuer")
	if err != nil {
		return nil, errConfiguration
	}
	credential.Close()
	binding := config.TrustBinding()
	verifier, err := p.NewManagementVerifier(binding, config.Roots)
	if err != nil {
		return nil, errConfiguration
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	observation, err := clock.Observe(bounded)
	elapsed := time.Since(start)
	if err != nil {
		return nil, err
	}
	if err := bounded.Err(); err != nil {
		return nil, err
	}
	if observation.UTC.IsZero() || observation.UTC.Location() != time.UTC || observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-observation.Uncertainty {
		return nil, errConfiguration
	}
	identity, err := verifier.VerifyCommandIssuerCertificate(wire, observation.UTC.Add(elapsed))
	if err != nil {
		return nil, errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	issuer := &ExecIssuer{key: private, certificate: identity.Wire(), identity: identity, binding: binding}
	issuer.self = issuer
	installed = true
	return issuer, nil
}

func (i *ExecIssuer) readLock(ctx context.Context) error {
	if ctx == nil || i == nil || i.self != i {
		return errConfiguration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.RLock()
	if err := ctx.Err(); err != nil {
		i.mu.RUnlock()
		return err
	}
	if i.closed {
		i.mu.RUnlock()
		return errConfiguration
	}
	return nil
}

func (i *ExecIssuer) Certificate(ctx context.Context) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	return bytes.Clone(i.certificate), nil
}

func (i *ExecIssuer) selected(digest string, claims p.ExecStartTicketClaims) bool {
	c := claims.Context
	binding := p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
	return digest == i.identity.Digest() && c.IssuerCertificateDigest == digest && c.IssuerCertificateID == i.identity.CertificateID() && binding == i.binding && !claims.NotBefore.Before(i.identity.NotBefore()) && !claims.NotAfter.After(i.identity.NotAfter())
}

func (i *ExecIssuer) SignStart(ctx context.Context, digest string, claims p.ExecStartTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if claims.Purpose != "operation_exec_start" || !i.selected(digest, claims) {
		return nil, errConfiguration
	}
	wire, err := p.SignExecStartTicket(i.key, i.identity, claims)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

func (i *ExecIssuer) SignRenew(ctx context.Context, digest string, claims p.ExecRenewTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if claims.Purpose != "operation_exec_renew" || !i.selected(digest, p.ExecStartTicketClaims(claims)) {
		return nil, errConfiguration
	}
	wire, err := p.SignExecRenewTicket(i.key, claims)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

func (i *ExecIssuer) Close() error {
	if i == nil {
		return nil
	}
	if i.self != i {
		return errConfiguration
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	clear(i.key)
	return nil
}
