package etcd

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Repeated references to one compact backing do not retain additional bytes.
// Equal contents in separate allocations still consume separate ownership.
// These pure counter tests grant no native metadata or effect permission.
func TestCleanupDetachedNoEffectRetainedStringBacking(t *testing.T) {
	t.Run("exact backing alias charged once", func(t *testing.T) {
		backing := strings.Clone(strings.Repeat("s", 4096))
		alias := backing
		defer runtime.KeepAlive(backing)

		require.Equal(t, reflect.ValueOf(backing).Pointer(), reflect.ValueOf(alias).Pointer())
		bytes, err := cleanupDetachedNoEffectGraphBytes(backing, alias)
		require.NoError(t, err)
		require.Equal(t, uint64(len(backing)), bytes, "the same backing and length must be charged once")
	})

	t.Run("independent equal contents charged separately", func(t *testing.T) {
		backing := strings.Clone(strings.Repeat("s", 4096))
		independent := strings.Clone(backing)
		defer runtime.KeepAlive(backing)
		defer runtime.KeepAlive(independent)

		require.Equal(t, backing, independent)
		require.NotEqual(t, reflect.ValueOf(backing).Pointer(), reflect.ValueOf(independent).Pointer())
		bytes, err := cleanupDetachedNoEffectGraphBytes(backing, independent)
		require.NoError(t, err)
		require.Equal(t, uint64(len(backing)+len(independent)), bytes, "contents cannot be used as a backing identity")
	})
}
