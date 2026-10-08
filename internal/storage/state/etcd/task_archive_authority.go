package etcd

import (
	"context"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"reflect"
)

// TaskWorkspaceArchiveIssuer is operator supplied, immutable, context honoring
// and concurrency safe. The digest selects the exact copied certificate.
// Construction invokes neither callback and retains no signing private key.
type TaskWorkspaceArchiveIssuer interface {
	Certificate(context.Context) ([]byte, error)
	SignWorkspaceArchive(context.Context, string, p.TaskWorkspaceArchiveTicketClaims) ([]byte, error)
}

func taskWorkspaceArchiveAuthority(o Options) (*p.ManagementVerifier, error) {
	if o.TaskWorkspaceArchiveIssuer == nil && (o.PublicationTrust == nil || o.Clock == nil) {
		return nil, nil
	}
	if o.TaskWorkspaceArchiveIssuer != nil {
		v := reflect.ValueOf(o.TaskWorkspaceArchiveIssuer)
		switch v.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if v.IsNil() {
				return nil, fmt.Errorf("%w: nil workspace archive issuer", ErrInvalidConfiguration)
			}
		}
	}
	publication, err := publicationVerifier(o)
	if err != nil {
		return nil, err
	}
	if publication == nil {
		return nil, fmt.Errorf("%w: workspace archive issuer requires publication trust and controlled clock", ErrInvalidConfiguration)
	}
	v, err := p.NewManagementVerifier(p.TrustBinding{Namespace: o.Namespace.Root(), AuthorityID: o.PublicationTrust.AuthorityID, Target: o.PublicationTrust.Target, RestoreEpoch: o.Identity.RestoreEpoch}, o.PublicationTrust.Roots)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace archive trust: %v", ErrInvalidConfiguration, err)
	}
	return v, nil
}
