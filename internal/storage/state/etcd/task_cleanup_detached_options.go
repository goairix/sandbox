package etcd

import (
	"fmt"
	"strings"

	"github.com/goairix/sandbox/internal/kubecontract"
)

// DetachedCleanupOptions opts into authenticated detached/plain source
// classification. It does not start a cleanup consumer or authorize effects.
type DetachedCleanupOptions struct {
	// PinnedPlainImage is a release image pinned by a lowercase sha256 digest,
	// limited to 4096 bytes before image parsing. Its fixed plain contract must
	// equal EnrolledPublication.ContractDigest. New retains an owned copy.
	PinnedPlainImage string
}

// detachedCleanupConfig is immutable after New. The digest is derived from the
// pinned executable image, never accepted as independent evidence of plainness.
type detachedCleanupConfig struct {
	pinnedPlainImage string
	contractDigest   string
}

func copyDetachedCleanupOptions(o Options) (*detachedCleanupConfig, error) {
	if o.DetachedCleanup == nil {
		return nil, nil
	}

	image := o.DetachedCleanup.PinnedPlainImage
	if len(image) == 0 || len(image) > 4096 {
		return nil, fmt.Errorf("%w: detached cleanup image must be 1 through 4096 bytes", ErrInvalidConfiguration)
	}
	contract, err := kubecontract.EnrolledPlainContractDigest(image)
	if err != nil {
		return nil, fmt.Errorf("%w: detached cleanup requires a pinned plain release image", ErrInvalidConfiguration)
	}
	if o.EnrolledPublication == nil || o.EnrolledPublication.ContractDigest != contract {
		return nil, fmt.Errorf("%w: detached cleanup plain contract differs from enrolled publication", ErrInvalidConfiguration)
	}

	// Cloning also avoids retaining a large caller string's backing allocation
	// when the validated image itself was only a small substring of that input.
	return &detachedCleanupConfig{pinnedPlainImage: strings.Clone(image), contractDigest: strings.Clone(contract)}, nil
}
