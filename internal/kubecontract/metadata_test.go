package kubecontract

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestMetadataNamesUseProjectNamespace(t *testing.T) {
	tests := map[string]string{
		"backend fingerprint": BackendFingerprintAnnotation,
		"drain protocol":      DrainProtocolAnnotation,
		"cleanup protocol":    CleanupProtocolAnnotation,
		"FUSE cleanup":        FUSERuntimeCleanupFinalizer,
	}
	want := map[string]string{
		"backend fingerprint": "goairix.github.io/sandbox-backend-fingerprint",
		"drain protocol":      "goairix.github.io/sandbox-drain-protocol",
		"cleanup protocol":    "goairix.github.io/sandbox-cleanup-protocol",
		"FUSE cleanup":        "goairix.github.io/sandbox-fuse-runtime-cleanup",
	}

	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want[name], value)
			require.Empty(t, validation.IsQualifiedName(value))
		})
	}
}
