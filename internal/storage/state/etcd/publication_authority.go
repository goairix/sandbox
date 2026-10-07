package etcd

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"reflect"
	"time"
)

// Aliases preserve existing AuthorityClock provider method signatures.
type ClockObservation = controlprotocol.ClockObservation
type AuthorityClock = controlprotocol.AuthorityClock

type RuntimePublicationTrust struct {
	AuthorityID, Target string
	Roots               []ed25519.PublicKey
}

func publicationVerifier(o Options) (*controlprotocol.PublicationVerifier, error) {
	if o.PublicationTrust == nil && o.Clock == nil {
		return nil, nil
	}
	if o.PublicationTrust == nil || o.Clock == nil {
		return nil, fmt.Errorf("%w: publication trust and controlled clock must be configured together", ErrInvalidConfiguration)
	}
	value := reflect.ValueOf(o.Clock)
	switch value.Kind() {
	case reflect.Chan, reflect.Pointer, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice:
		if value.IsNil() {
			return nil, fmt.Errorf("%w: nil publication clock", ErrInvalidConfiguration)
		}
	}
	if o.PublicationTrust.AuthorityID != o.Identity.RuntimeID {
		return nil, fmt.Errorf("%w: publication authority does not match runtime identity", ErrInvalidConfiguration)
	}
	verifier, err := controlprotocol.NewPublicationVerifier(controlprotocol.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}, o.PublicationTrust.Roots)
	if err != nil {
		return nil, fmt.Errorf("%w: publication trust: %v", ErrInvalidConfiguration, err)
	}
	return verifier, nil
}
func (b *Backend) observePublicationClock(ctx context.Context) (time.Time, error) {
	if ctx == nil || b.publicationVerifier == nil || b.authorityClock == nil {
		return time.Time{}, fmt.Errorf("%w: publication authority unavailable", ErrInvalidConfiguration)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	started := time.Now()
	observation, err := b.authorityClock.Observe(bounded)
	elapsed := time.Since(started)
	if err != nil {
		return time.Time{}, err
	}
	if err := bounded.Err(); err != nil {
		return time.Time{}, err
	}
	if observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-observation.Uncertainty || !validDomainExpiry(observation.UTC) || observation.UTC.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("%w: invalid controlled clock observation", ErrInvalidRecord)
	}
	now := observation.UTC.Add(elapsed)
	if !validDomainExpiry(now) {
		return time.Time{}, ErrInvalidRecord
	}
	return now, nil
}
