package etcd

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVisitPrefixValuesKeepsOneRevisionAcrossPages(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for index := range 300 {
		key := fmt.Sprintf("owners/%03d", index)
		require.NoError(t, store.Set(ctx, key, []byte(key), 0))
	}

	visited := make([]string, 0, 300)
	err := store.VisitPrefixValues(ctx, "owners/", func(key string, value []byte) (bool, error) {
		if len(visited) == 0 {
			require.NoError(t, store.Set(ctx, "owners/zzz", []byte("late"), 0))
			require.NoError(t, store.Delete(ctx, "owners/299"))
		}
		require.Equal(t, key, string(value))
		visited = append(visited, key)
		return false, nil
	})
	require.NoError(t, err)
	require.Len(t, visited, 300)
	require.Equal(t, "owners/299", visited[299])
	require.NotContains(t, visited, "owners/zzz")

	var first string
	err = store.VisitPrefixValues(ctx, "owners/", func(key string, _ []byte) (bool, error) {
		first = key
		return true, nil
	})
	require.NoError(t, err)
	require.Equal(t, "owners/000", first)
}
