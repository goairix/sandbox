//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// httpTerminalLookupNative starts only after the original production worker
// has released the local runtime. The business Backend has publication trust
// and a clock, but no physical cleanup provider; terminal reads must need only
// the authenticated durable release evidence produced by that worker.
func httpTerminalLookupNative(t *testing.T, ctx context.Context, f httpDestroyNativeContext, client *http.Client, origin, sandboxID string, generation int64) {
	t.Helper()
	path := "/api/v1/sandboxes/" + sandboxID
	var absent types.ErrorResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 404, &absent)
	require.Equal(t, "not_found", absent.Code)
	released, err := f.backend.ReadSandboxReleased(ctx, sandboxID)
	require.NoError(t, err)
	require.NotNil(t, released)

	// Reuse commits a new publishing quartet, without creating a second Pod or
	// launcher. Its metadata is enough to prove that the old sandbox's absence
	// neither follows nor changes the new owner; it makes no health claim.
	input := state.AcquireIntentInput{
		Workspace: f.workspace, Principal: f.request.Principal,
		IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(),
		IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(),
		Payload: json.RawMessage(`{"native":"terminal-lookup-successor"}`),
		Now:     time.Now().UTC(), TTL: time.Minute,
	}
	acquired, err := f.backend.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, state.AcquireCreated, acquired.Disposition)
	require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
	require.NoError(t, acquired.GuardCleanupError)

	view, err := f.backend.ReadSandboxView(ctx, input.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, view)
	records := view.Records()
	require.Equal(t, input.SandboxID, records.Owner.SandboxID)
	require.Equal(t, generation+1, records.Owner.Generation)
	require.Nil(t, records.Owner.Runtime)
	require.Equal(t, state.PhasePublishing, records.Control.Phase)
	require.Nil(t, records.Control.Runtime)
	ns, err := state.NewNamespace(f.identity.Prefix, f.identity.AuthorityID, f.identity.Cell)
	require.NoError(t, err)
	partition := fmt.Sprintf("%sp/%02x/", ns.Root(), f.workspace.Partition())
	keys := []string{
		partition + "workspaces/" + f.workspace.Hash() + "/owner",
		partition + "workspaces/" + f.workspace.Hash() + "/fence",
		ns.Root() + "indexes/sandbox/" + input.SandboxID,
		partition + "controls/" + input.SandboxID,
	}
	before := make([]*clientv3.GetResponse, len(keys))
	for i, key := range keys {
		before[i], err = httpDestroyNativeGet(ctx, f.raw, key, clientv3.WithRev(view.Revision()))
		require.NoError(t, err)
		require.Len(t, before[i].Kvs, 1)
		require.Zero(t, before[i].Kvs[0].Lease)
	}

	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 404, &absent)
	require.Equal(t, "not_found", absent.Code)
	released, err = f.backend.ReadSandboxReleased(ctx, sandboxID)
	require.NoError(t, err)
	require.NotNil(t, released, "original release remains authenticated after workspace reuse")
	for i, key := range keys {
		after, err := httpDestroyNativeGet(ctx, f.raw, key)
		require.NoError(t, err)
		require.Len(t, after.Kvs, 1)
		require.Equal(t, before[i].Kvs[0], after.Kvs[0], "old sandbox lookup preserves successor quartet bytes and MVCC fields")
	}
	t.Log("public GET local terminal: real live GET200 before DELETE202, authenticated release then GET404; old ID stays404 after real AcquireIntent reuse, successor publishing quartet bytes/MVCC unchanged; no successor health or release claim")
}
