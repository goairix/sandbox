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

// CleanupIssuer owns copied mounted signing material for the four original
// cleanup purposes. Backend borrows it until Backend.Close has joined all work.
// It signs attribution only; it cannot replace an original live effect guard.
type CleanupIssuer struct {
	self   *CleanupIssuer
	mu     sync.RWMutex
	closed bool

	key         ed25519.PrivateKey
	certificate []byte
	identity    p.CommandIssuerIdentity
	binding     p.TrustBinding
}

// NewCleanupIssuer authenticates the mounted key/certificate against the
// launcher's public roots and bounded authority clock, then owns independent
// copies. It registers no metadata and never requires a root signing key.
func NewCleanupIssuer(ctx context.Context, key ed25519.PrivateKey, certificate []byte, config launcherconfig.Config, clock p.AuthorityClock) (*CleanupIssuer, error) {
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
	verifier, err := p.NewManagementVerifier(config.TrustBinding(), config.Roots)
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
	i := &CleanupIssuer{key: private, certificate: identity.Wire(), identity: identity, binding: config.TrustBinding()}
	i.self = i
	installed = true
	return i, nil
}

// The read lock spans the actual signer. Close cannot clear its key until all
// original signers return; cancellation never detaches a signing operation.
func (i *CleanupIssuer) readLock(ctx context.Context) error {
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

func (i *CleanupIssuer) selected(digest string, c p.TaskCloseDataContext, before, after time.Time) bool {
	binding := p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
	return digest == i.identity.Digest() && c.IssuerCertificateDigest == digest && c.IssuerCertificateID == i.identity.CertificateID() && binding == i.binding && !before.Before(i.identity.NotBefore()) && !after.After(i.identity.NotAfter())
}

// Certificate returns owned public bytes, valid independently of later Close.
func (i *CleanupIssuer) Certificate(ctx context.Context) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	return bytes.Clone(i.certificate), nil
}

// SignCloseData signs only the selected issuer's original close-data purpose.
func (i *CleanupIssuer) SignCloseData(ctx context.Context, digest string, c p.TaskCloseDataTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if c.Purpose != "task_close_data" || !i.selected(digest, c.Context, c.NotBefore, c.NotAfter) {
		return nil, errConfiguration
	}
	wire, err := p.SignTaskCloseDataTicket(i.key, i.identity, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

// SignQuiesceUsers retains the original historical-close linkage validation.
func (i *CleanupIssuer) SignQuiesceUsers(ctx context.Context, digest string, c p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if c.Purpose != "task_quiesce_users" || !i.selected(digest, c.Context.Current, c.NotBefore, c.NotAfter) {
		return nil, errConfiguration
	}
	wire, err := p.SignTaskUserQuiescenceTicket(i.key, i.identity, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

// SignWorkspaceArchive bounds the entire archive session by the certificate,
// not merely its shorter command ticket interval.
func (i *CleanupIssuer) SignWorkspaceArchive(ctx context.Context, digest string, c p.TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if c.Purpose != "task_workspace_archive" || !i.selected(digest, c.Context.Current, c.NotBefore, c.SessionNotAfter) {
		return nil, errConfiguration
	}
	wire, err := p.SignTaskWorkspaceArchiveTicket(i.key, i.identity, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

// SignRuntimeTermination signs the original reserved observer request only;
// the protocol signer preserves its intent, enrollment and node constraints.
func (i *CleanupIssuer) SignRuntimeTermination(ctx context.Context, digest string, c p.TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	if err := i.readLock(ctx); err != nil {
		return nil, err
	}
	defer i.mu.RUnlock()
	if c.Purpose != "task_runtime_terminate" || !i.selected(digest, c.Current, c.NotBefore, c.NotAfter) {
		return nil, errConfiguration
	}
	wire, err := p.SignTaskRuntimeTerminationTicket(i.key, i.identity, c)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return wire, err
}

// Close joins actual signing readers before clearing owned key bytes. It is
// idempotent and never clears the caller's original mounted-key backing slice.
func (i *CleanupIssuer) Close() error {
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
