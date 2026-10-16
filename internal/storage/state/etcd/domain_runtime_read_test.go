package etcd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSandboxRuntimeViewBindingNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "runtime-view")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	view, err := b.ReadSandboxView(ctx, in.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, view)
	copy := *view
	foreign := *b
	for _, invalid := range []*SandboxReadView{nil, {}, &copy} {
		result, err := b.ReadSandboxRuntime(ctx, invalid)
		require.ErrorIs(t, err, ErrInvalidRecord)
		require.Nil(t, result)
	}
	result, err := foreign.ReadSandboxRuntime(ctx, view)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, result)
	// A same-value control rewrite invalidates the original MOD pin.
	got, err := raw.Get(ctx, view.keys[3])
	require.NoError(t, err)
	_, err = raw.Put(ctx, view.keys[3], string(got.Kvs[0].Value))
	require.NoError(t, err)
	result, err = b.ReadSandboxRuntime(ctx, view)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, result)
	// The new locator path must not weaken existing workspace APIs.
	_, err = b.LoadRuntimePublicationEnrolled(ctx, WorkspaceIdentity{}, in.IntentID)
	require.Error(t, err)
	_, err = b.LoadRuntimeDispatch(ctx, WorkspaceIdentity{}, in.IntentID)
	require.Error(t, err)
}
