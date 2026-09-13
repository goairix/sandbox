package kubernetes

import (
	"context"
	"fmt"

	"github.com/goairix/sandbox/internal/runtime"
)

func exactPodCommand(ctx context.Context, podName string, command []string) ([]string, error) {
	ref, exact := runtime.ExactRuntimeRefFromContext(ctx)
	if !exact {
		return command, nil
	}
	if ref.Validate() != nil || ref.ID != podName {
		return nil, runtime.ErrInvalidRuntimeRef
	}
	// SANDBOX_POD_UID is immutable kubelet-injected container environment from
	// metadata.uid. A replacement reached after the API-side GET cannot pass.
	guard := fmt.Sprintf("[ \"${SANDBOX_POD_UID:-}\" = %s ] || exit 125; exec \"$@\"", shellEscape(ref.UID))
	argv := []string{"sh", "-c", guard, "sandbox-exact"}
	return append(argv, command...), nil
}
