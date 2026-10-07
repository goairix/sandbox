package etcd

import (
	"context"
	"fmt"
	"reflect"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// TaskQuiescenceIssuer is an operator-provisioned, immutable, context-honoring and
// concurrency-safe dependency. SignQuiesceUsers must select the exact certificate
// digest supplied by the backend and sign only the fixed quiesce-users purpose. Backend
// construction does not invoke either method or retain authority private keys.
type TaskQuiescenceIssuer interface {
	Certificate(context.Context) ([]byte, error)
	SignQuiesceUsers(context.Context, string, controlprotocol.TaskUserQuiescenceTicketClaims) ([]byte, error)
}

func taskQuiescenceAuthority(o Options) (*controlprotocol.ManagementVerifier, error) {
	if o.TaskQuiescenceIssuer == nil && (o.PublicationTrust == nil || o.Clock == nil) {
		return nil, nil
	}
	value := reflect.ValueOf(o.TaskQuiescenceIssuer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil, fmt.Errorf("%w: nil task quiescence issuer", ErrInvalidConfiguration)
		}
	}
	// Reuse publication trust/clock validation without observing the clock.
	publication, err := publicationVerifier(o)
	if err != nil {
		return nil, err
	}
	if publication == nil {
		return nil, fmt.Errorf("%w: task quiescence issuer requires publication trust and controlled clock", ErrInvalidConfiguration)
	}
	verifier, err := controlprotocol.NewManagementVerifier(controlprotocol.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}, o.PublicationTrust.Roots)
	if err != nil {
		return nil, fmt.Errorf("%w: task quiescence issuer trust: %v", ErrInvalidConfiguration, err)
	}
	return verifier, nil
}
