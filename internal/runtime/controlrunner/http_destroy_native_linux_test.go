//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Each subtest is selected in a separate container. It retains the original
// cold-service fixture's real creation/enrollment and gets its own 120s budget.
func TestHTTPDestroyNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_DESTROY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP destroy Linux fixture")
	}
	t.Run("local-accepted", func(t *testing.T) {
		runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{destroyLocal: true})
	})
	t.Run("local-terminal", func(t *testing.T) {
		// Public GET renders the real business capsule created by this variant.
		runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{destroyLocal: true, destroyTerminal: true, withBusiness: true})
	})
	t.Run("detached-refused", func(t *testing.T) {
		runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{detached: true, destroyDetached: true})
	})
	t.Run("detached-terminal", func(t *testing.T) {
		runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{detached: true, withBusiness: true, destroyDetached: true, destroyTerminal: true})
	})
}

type httpDestroyNativeContext struct {
	service         *provisioning.PlainColdService
	backend         *state.Backend
	raw             *clientv3.Client
	options         provisioning.PlainColdOptions
	request         provisioning.PlainColdRequest
	workspace       state.WorkspaceIdentity
	identity        state.Identity
	directory       string
	observerAddress string
	clockKey        ed25519.PrivateKey
	provider        *httpDestroyNativeProvider
	terminal        bool
	detached        bool
}

type httpDestroyNativeResponse struct {
	Code       string                             `json:"code"`
	State      provisioning.PlainColdDestroyState `json:"state"`
	SandboxID  string                             `json:"sandbox_id"`
	RequestID  string                             `json:"request_id"`
	Generation int64                              `json:"generation"`
	Runtime    p.RuntimeReference                 `json:"runtime"`
	Task       state.TaskReference                `json:"task"`
	Stage      state.StageReference               `json:"stage"`
}

func exerciseHTTPDestroyNative(t *testing.T, ctx context.Context, f httpDestroyNativeContext, local bool) {
	t.Helper()
	settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000}
	gateway, err := etcdgateway.New(f.service, settings)
	require.NoError(t, err)
	settings.Zero()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverCtx, stopServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
	defer func() {
		stopServer()
		require.NoError(t, <-serverDone, "HTTP socket/handler owners physically joined")
	}()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}}
	defer client.CloseIdleConnections()
	origin := "http://" + listener.Addr().String()
	if !local {
		if f.detached {
			httpDestroyNativeDetachedTerminal(t, ctx, f, client, origin)
		} else {
			httpDestroyNativeDetached(t, ctx, f, client, origin)
		}
		return
	}

	// The real producer writes the actual local v2 descriptor; neither the
	// destroy helper nor an arbitrary cleanup payload fabricates that history.
	created, err := f.service.Create(ctx, f.workspace, f.request)
	require.NoError(t, err)
	require.Equal(t, provisioning.PlainColdCompleted, created.State)
	read, err := f.backend.ReadSandboxSnapshot(ctx, created.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, read)
	require.EqualValues(t, 2, read.Snapshot.Version)
	require.NotNil(t, read.Snapshot.LocalCleanup)
	require.Equal(t, "local", read.Snapshot.LocalCleanup.Provider)
	original := read.View.Records()
	current := original.Control
	require.NotNil(t, current.Runtime)
	ns, err := state.NewNamespace(f.identity.Prefix, f.identity.AuthorityID, f.identity.Cell)
	require.NoError(t, err)
	// Placement and fence survive owner release. Capture their exact raw
	// bytes and MVCC fields at the authenticated view's coherent revision.
	retainedKeys := []string{
		ns.Root() + "indexes/sandbox/" + created.SandboxID,
		fmt.Sprintf("%sp/%02x/workspaces/%s/fence", ns.Root(), f.workspace.Partition(), f.workspace.Hash()),
	}
	retained := make([]*clientv3.GetResponse, len(retainedKeys))
	for i, key := range retainedKeys {
		retained[i], err = httpDestroyNativeGet(ctx, f.raw, key, clientv3.WithRev(read.View.Revision()))
		require.NoError(t, err)
		require.Len(t, retained[i].Kvs, 1)
		require.Zero(t, retained[i].Kvs[0].Lease)
	}
	var placement state.SandboxPlacementRecord
	require.NoError(t, json.Unmarshal(retained[0].Kvs[0].Value, &placement))
	require.Equal(t, original.Placement, &placement)
	var fence state.WorkspaceFenceRecord
	require.NoError(t, json.Unmarshal(retained[1].Kvs[0].Value, &fence))
	require.Equal(t, original.Fence, &fence)

	indices, err := httpDestroyNativeGet(ctx, f.raw, ns.Root()+"indexes/runtime/", clientv3.WithPrefix(), clientv3.WithLimit(2))
	require.NoError(t, err)
	require.Len(t, indices.Kvs, 1)
	var originalIndex state.RuntimeIndexRecord
	require.NoError(t, json.Unmarshal(indices.Kvs[0].Value, &originalIndex))
	require.Equal(t, created.SandboxID, originalIndex.SandboxID)
	require.Equal(t, *current.Runtime, originalIndex.Runtime)
	indexKey := string(indices.Kvs[0].Key)
	request := p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", "umask 027; mkdir -p /workspace/nested; printf 'HTTP delete archive\n' > /workspace/nested/data"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, UID: 1000, GID: 1000, WorkDir: "/workspace", TimeoutSeconds: 5}
	var stdout, stderr bytes.Buffer
	seed, err := f.service.Exec(ctx, created.SandboxID, uuid.NewString(), request, &stdout, &stderr)
	require.NoError(t, err, "%s", stderr.String())
	require.Equal(t, provisioning.PlainExecLocalTerminal, seed.State)
	require.True(t, seed.Receipt.DrainConfirmed())
	require.Zero(t, seed.Receipt.RootWaitStatus())

	path := "/api/v1/sandboxes/" + created.SandboxID
	if f.terminal {
		var live types.SandboxResponse
		httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 200, &live)
		require.Equal(t, created.SandboxID, live.ID)
	}

	var accepted httpDestroyNativeResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 202, &accepted)
	require.Equal(t, "destroy_pending", accepted.Code)
	require.Equal(t, provisioning.PlainColdDestroyPending, accepted.State)
	require.Equal(t, created.SandboxID, accepted.SandboxID)
	require.Equal(t, current.Generation, accepted.Generation)
	require.Equal(t, p.RuntimeReference(*current.Runtime), accepted.Runtime)
	require.NotEmpty(t, accepted.RequestID)
	require.Equal(t, accepted.RequestID, accepted.Stage.RequestID)
	require.Equal(t, "destroy", accepted.Stage.StageID)
	require.NoError(t, accepted.Task.Validate())
	httpDestroyNativeBirth(t, ctx, f, accepted)
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 409, nil)

	httpDestroyNativeAutomatic(t, ctx, f, accepted, read.Snapshot.LocalCleanup)
	owner, err := f.backend.LoadOwner(ctx, f.workspace)
	require.NoError(t, err)
	require.Nil(t, owner)
	controlKey := fmt.Sprintf("%sp/%02x/controls/%s", ns.Root(), f.workspace.Partition(), created.SandboxID)
	control, err := httpDestroyNativeGet(ctx, f.raw, controlKey)
	require.NoError(t, err)
	require.Empty(t, control.Kvs, "exact original control is released")
	index, err := httpDestroyNativeGet(ctx, f.raw, indexKey)
	require.NoError(t, err)
	require.Empty(t, index.Kvs, "exact original runtime index is released")
	for i, key := range retainedKeys {
		after, err := httpDestroyNativeGet(ctx, f.raw, key)
		require.NoError(t, err)
		require.Len(t, after.Kvs, 1)
		require.Equal(t, retained[i].Kvs[0], after.Kvs[0], "retained placement/fence bytes and MVCC fields must remain unchanged")
	}
	// Release deliberately removes only owner/control/runtime index. Discovery
	// still sees retained placement, so the all-four-point reader must fail
	// closed. Only the separate authenticated release reader can establish 404.
	view, err := f.backend.ReadSandboxView(ctx, created.SandboxID)
	require.ErrorIs(t, err, state.ErrCorruptRecord)
	require.Nil(t, view)
	require.Greater(t, f.provider.patches.Load(), int32(0))
	require.EqualValues(t, 1, f.provider.deletes.Load())
	// Cold quota settlement is a separate existing API, not wired into this
	// automatic worker. Preserve and report its original occupied slot instead
	// of introducing a test-only refund that hides the production gap.
	quota, err := httpDestroyNativeGet(ctx, f.raw, accepted.Task.Namespace+"quota/07/slots/00000000")
	require.NoError(t, err)
	require.Len(t, quota.Kvs, 1)
	require.Contains(t, string(quota.Kvs[0].Value), `"occupied":true`)
	t.Log("HTTP DELETE local complete: actual service Create/Exec, atomic Task/P, automatic physical release/Pgone; cold quota remains occupied (separate production wiring gap)")
	if f.terminal {
		httpTerminalLookupNative(t, ctx, f, client, origin, created.SandboxID, current.Generation)
	} else {
		t.Log("this variant does not exercise terminal GET; the original four-point reader remains fail-closed")
	}
}

// requireHTTPDestroyNativeBridgeJoined checks the original physical owners
// before classifying only the final WebSocket Close frame. Management queries
// may close after their authenticated response without reading that tail frame;
// earlier IO, status, context, stderr and child failures remain test failures.
func requireHTTPDestroyNativeBridgeJoined(t *testing.T, bridge *startupBridgeFixture) {
	t.Helper()
	bridge.mu.Lock()
	attempted := bridge.attempted
	bridge.mu.Unlock()
	require.True(t, attempted, "production bridge client must issue real exec")
	<-bridge.done
	bridge.wg.Wait()
	require.True(t, bridge.waited, "original Cmd.Wait must run after all pipe readers join")
	require.NoError(t, bridge.waitErr)
	require.True(t, bridge.stdinEOF, "v5 FIN must become actual child stdin EOF")
	require.Positive(t, bridge.stdout, "actual bridge stdout must carry TLS response")
	require.True(t, bridge.ioJoined, "original IO, child, context and empty stderr checks must all succeed")
	require.True(t, bridge.successWritten, "original Success status frame must be written successfully")
	if bridge.result != nil {
		require.Same(t, bridge.finalCloseErr, bridge.result, "only the original final Close frame error may be classified")
		require.ErrorIs(t, bridge.result, syscall.EPIPE)
		t.Logf("HTTP destroy original bridge IO/Cmd.Wait/Success joined; final WebSocket Close frame only: %v", bridge.result)
		return
	}
	require.NoError(t, bridge.finalCloseErr)
	t.Log("HTTP destroy original bridge Cmd.Wait=exit0; all IO/Success/Close frames joined")
}

func httpDestroyNativeDetached(t *testing.T, ctx context.Context, f httpDestroyNativeContext, client *http.Client, origin string) {
	t.Helper()
	var created types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", uuid.NewString(), types.CreateSandboxRequest{Mode: "persistent"}, true), 201, &created)
	ns, err := state.NewNamespace(f.identity.Prefix, f.identity.AuthorityID, f.identity.Cell)
	require.NoError(t, err)
	before := httpDestroyNativeNamespace(t, ctx, f.raw, ns.Root())
	path := "/api/v1/sandboxes/" + created.ID
	wire := httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 501, nil)
	require.Contains(t, string(wire), `"code":"unsupported_cleanup_profile"`)
	after := httpDestroyNativeNamespace(t, ctx, f.raw, ns.Root())
	require.Equal(t, before, after, "profile refusal writes no control, Stage, Task, P or other namespace metadata")
	var current types.SandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 200, &current)
	require.Equal(t, created.SandboxResponse, current)
	var result types.ExecResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, path+"/exec", "", types.ExecRequest{Language: "bash", Code: "printf 'still-open'; exit 7"}, true), 200, &result)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, "still-open", result.Stdout)
	t.Logf("HTTP DELETE detached refusal: complete bounded namespace unchanged (%d records), subsequent authenticated GET and actual Exec exit7 passed", len(before))
}

// One original business creation and DELETE produce the sole Task/P. The
// mounted production worker must discover that Task itself, acquire its managed
// Claim, join real runtime effects and refund the original quota before P goes.
// Native rows below are assertions only; none is fed back as effect authority.
func httpDestroyNativeDetachedTerminal(t *testing.T, ctx context.Context, f httpDestroyNativeContext, client *http.Client, origin string, beforeDestroy ...func(string)) {
	t.Helper()
	require.LessOrEqual(t, len(beforeDestroy), 1)
	idempotency := uuid.NewString()
	var created types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", idempotency, types.CreateSandboxRequest{Mode: "persistent"}, true), 201, &created)
	read, err := f.backend.ReadSandboxSnapshot(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, read)
	require.NotNil(t, read.Snapshot)
	require.NotNil(t, read.View)
	require.EqualValues(t, 1, read.Snapshot.Version)
	require.Nil(t, read.Snapshot.LocalCleanup)
	current := read.View.Records().Control
	require.NotNil(t, current)
	require.NotNil(t, current.Runtime)
	f.workspace, err = f.backend.DetachedIdentity(f.options.Business.Principal, idempotency)
	require.NoError(t, err)
	require.Equal(t, f.workspace.Hash(), current.WorkspaceHash)
	f.request.IntentID, f.request.SandboxID = current.IntentID, created.ID
	ns, err := state.NewNamespace(f.identity.Prefix, f.identity.AuthorityID, f.identity.Cell)
	require.NoError(t, err)
	retainedKeys := []string{ns.Root() + "indexes/sandbox/" + created.ID, fmt.Sprintf("%sp/%02x/workspaces/%s/fence", ns.Root(), f.workspace.Partition(), f.workspace.Hash())}
	retained := make([]*clientv3.GetResponse, len(retainedKeys))
	for i, key := range retainedKeys {
		retained[i], err = httpDestroyNativeGet(ctx, f.raw, key, clientv3.WithRev(read.View.Revision()))
		require.NoError(t, err)
		require.Len(t, retained[i].Kvs, 1)
	}
	indices, err := httpDestroyNativeGet(ctx, f.raw, ns.Root()+"indexes/runtime/", clientv3.WithPrefix(), clientv3.WithLimit(2))
	require.NoError(t, err)
	require.Len(t, indices.Kvs, 1)
	indexKey := string(indices.Kvs[0].Key)
	var stdout, stderr bytes.Buffer
	executed, err := f.service.Exec(ctx, created.ID, uuid.NewString(), p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", "printf 'detached user before destroy\\n'"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, UID: 1000, GID: 1000, WorkDir: "/workspace", TimeoutSeconds: 5}, &stdout, &stderr)
	require.NoError(t, err, "%s", stderr.String())
	require.Equal(t, provisioning.PlainExecLocalTerminal, executed.State)
	require.NotNil(t, executed.Receipt)
	require.True(t, executed.Receipt.DrainConfirmed())
	require.Zero(t, executed.Receipt.RootWaitStatus())
	path := "/api/v1/sandboxes/" + created.ID
	var live types.SandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 200, &live)
	require.Equal(t, created.ID, live.ID)
	// The inventory variant adds one explicit real HTTP Exec here, while the
	// original target is still active. Other variants pass no step and retain
	// the original sequence; HTTP transport never injects extra operations.
	if len(beforeDestroy) == 1 {
		beforeDestroy[0](path)
	}
	var accepted httpDestroyNativeResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 202, &accepted)
	require.Equal(t, "destroy_pending", accepted.Code)
	require.Equal(t, provisioning.PlainColdDestroyPending, accepted.State)
	require.Equal(t, created.ID, accepted.SandboxID)
	require.Equal(t, current.Generation, accepted.Generation)
	require.Equal(t, p.RuntimeReference(*current.Runtime), accepted.Runtime)
	require.NoError(t, accepted.Task.Validate())
	httpDestroyNativeBirth(t, ctx, f, accepted)
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 409, nil)
	// Detached history has no local descriptor. The original worker gets only
	// its deployment storage configuration, never a manufactured local target.
	httpDestroyNativeAutomatic(t, ctx, f, accepted, nil)
	owner, err := f.backend.LoadOwner(ctx, f.workspace)
	require.NoError(t, err)
	require.Nil(t, owner)
	for _, key := range []string{fmt.Sprintf("%sp/%02x/controls/%s", ns.Root(), accepted.Task.Partition, created.ID), indexKey} {
		point, err := httpDestroyNativeGet(ctx, f.raw, key)
		require.NoError(t, err)
		require.Empty(t, point.Kvs)
	}
	for i, key := range retainedKeys {
		after, err := httpDestroyNativeGet(ctx, f.raw, key)
		require.NoError(t, err)
		require.Equal(t, retained[i].Kvs, after.Kvs, "release preserves actual placement/fence bytes and MVCC")
	}
	base := fmt.Sprintf("%sp/%02x/tasks/%s/", ns.Root(), accepted.Task.Partition, accepted.Task.TaskID)
	fact := coldQuotaReadOne(t, ctx, f.raw, base+"detached-noeffect")
	settlement, err := p.DecodeDetachedNoEffectSettlement(fact.Value)
	require.NoError(t, err)
	require.Equal(t, accepted.Task.TaskID, settlement.QuiescenceContext.TaskID)
	termination := coldQuotaReadOne(t, ctx, f.raw, base+"runtime-termination")
	intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Value)
	require.NoError(t, err)
	require.EqualValues(t, 2, intent.Version)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.NotEmpty(t, intent.TerminalProof)
	require.NotNil(t, intent.DetachedEffects)
	require.Equal(t, hashBytes(fact.Value), intent.DetachedEffects.Settlement.SHA256)
	seal := coldQuotaReadOne(t, ctx, f.raw, base+"owner-release")
	var released struct {
		Version uint32              `json:"version"`
		Task    state.TaskReference `json:"task"`
	}
	require.NoError(t, json.Unmarshal(seal.Value, &released))
	require.EqualValues(t, 2, released.Version)
	require.Equal(t, accepted.Task, released.Task)
	provider := coldQuotaReadOne(t, ctx, f.raw, base+"runtime-provider-cleanup")
	var cleanup struct {
		Slots []struct {
			State string `json:"state"`
		} `json:"slots"`
	}
	require.NoError(t, json.Unmarshal(provider.Value, &cleanup))
	require.Len(t, cleanup.Slots, 7)
	for i, slot := range cleanup.Slots {
		if i >= 2 && i <= 5 {
			require.Contains(t, []string{"complete", "skipped"}, slot.State)
		} else {
			require.Equal(t, "complete", slot.State)
		}
	}
	for _, suffix := range []string{"workspace-archive-command", "workspace-sync-local-progress", "workspace-sync-local-complete"} {
		point, err := httpDestroyNativeGet(ctx, f.raw, base+suffix)
		require.NoError(t, err)
		require.Empty(t, point.Kvs)
	}
	require.Positive(t, f.provider.patches.Load())
	require.EqualValues(t, 1, f.provider.deletes.Load())
	var absent types.ErrorResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 404, &absent)
	require.Equal(t, "not_found", absent.Code)
	proof, err := f.backend.ReadSandboxReleased(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, proof, "original production history reader authenticates the worker's v2 release")
	t.Log("HTTP detached Create201→DELETE202→original automatic managed Claim/drain/none/observer/provider/release→quota refund→S/P gone→GET404; all original workers/processes joined")
}

// The fixture has a fixed inventory, not arbitrary resident N. A complete
// snapshot must fit both bounds; truncation is never called a no-write proof.
func httpDestroyNativeNamespace(t *testing.T, ctx context.Context, raw *clientv3.Client, root string) []string {
	t.Helper()
	response, err := httpDestroyNativeGet(ctx, raw, root, clientv3.WithPrefix(), clientv3.WithLimit(512))
	require.NoError(t, err)
	require.False(t, response.More)
	require.EqualValues(t, len(response.Kvs), response.Count)
	var total int
	rows := make([]string, len(response.Kvs))
	for i, kv := range response.Kvs {
		total += len(kv.Key) + len(kv.Value)
		require.LessOrEqual(t, total, 1<<20)
		rows[i] = fmt.Sprintf("%s:%d:%d:%d:%d:%x", kv.Key, kv.CreateRevision, kv.ModRevision, kv.Version, kv.Lease, sha256.Sum256(kv.Value))
	}
	return rows
}

func httpDestroyNativeBirth(t *testing.T, ctx context.Context, f httpDestroyNativeContext, result httpDestroyNativeResponse) {
	t.Helper()
	partition := fmt.Sprintf("%sp/%02x/", result.Task.Namespace, result.Task.Partition)
	keys := []string{
		partition + "controls/" + result.SandboxID,
		partition + "tasks/" + result.Task.TaskID,
		partition + "cleanup-intents/" + result.Task.TaskID,
		partition + "sandboxes/" + result.SandboxID + "/cleanup-task",
		partition + "tasks/" + result.Task.TaskID + "/checkpoint",
		partition + "cleanup-pending/" + result.Task.TaskID,
		fmt.Sprintf("%sstages/%s/%s/%s/receipt", partition, result.Stage.RequestID, result.Stage.StageID, result.Stage.AttemptID),
	}
	var birth int64
	for i, key := range keys {
		response, err := httpDestroyNativeGet(ctx, f.raw, key)
		require.NoError(t, err)
		require.Len(t, response.Kvs, 1, "%s", key)
		kv := response.Kvs[0]
		require.Zero(t, kv.Lease)
		if i == 0 {
			birth = kv.ModRevision
			require.Contains(t, string(kv.Value), `"phase":"destroying"`)
		} else {
			require.Equal(t, birth, kv.CreateRevision)
			require.Equal(t, birth, kv.ModRevision)
		}
		if i == len(keys)-1 {
			require.Contains(t, string(kv.Value), `"outcome":"committed"`)
		}
	}
	guard, err := httpDestroyNativeGet(ctx, f.raw, partition+"attempts/"+result.Stage.AttemptID+"/guard")
	require.NoError(t, err)
	require.Empty(t, guard.Kvs, "HTTP returned only after original Stage release")
}

// This adds only external provider semantics to the original cold fixture.
// Mutating Kubernetes metadata cannot stand in for the observer's actual kill,
// exact UID proof, cgroup empty observation or original process Wait.
type httpDestroyNativeProvider struct {
	patches atomic.Int32
	deletes atomic.Int32
}

func (a *httpDestroyNativeProvider) handle(w http.ResponseWriter, r *http.Request, mu *sync.Mutex, pod **v1.Pod) bool {
	if r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if *pod == nil || r.URL.Path != "/api/v1/namespaces/runtimes/pods/"+(*pod).Name {
		w.WriteHeader(404)
		return true
	}
	actual := *pod
	if r.Method == http.MethodPatch {
		var patch []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&patch) != nil || len(patch) != 3 {
			w.WriteHeader(400)
			return true
		}
		var uid, version string
		var finalizers []string
		validPatch := json.Unmarshal(patch[0].Value, &uid) == nil &&
			json.Unmarshal(patch[1].Value, &version) == nil &&
			json.Unmarshal(patch[2].Value, &finalizers) == nil &&
			patch[0].Op == "test" && patch[0].Path == "/metadata/uid" &&
			patch[1].Op == "test" && patch[1].Path == "/metadata/resourceVersion" &&
			patch[2].Op == "add" && patch[2].Path == "/metadata/finalizers" &&
			uid == string(actual.UID) && version == actual.ResourceVersion
		if !validPatch {
			w.WriteHeader(409)
			return true
		}
		if len(finalizers) > 1 || len(finalizers) == 1 && finalizers[0] != kubecontract.EnrolledRetainFinalizer {
			w.WriteHeader(400)
			return true
		}
		a.patches.Add(1)
		actual.Finalizers = finalizers
		versionNumber, _ := strconv.Atoi(actual.ResourceVersion)
		actual.ResourceVersion = strconv.Itoa(versionNumber + 1)
		response := actual.DeepCopy()
		if actual.DeletionTimestamp != nil && len(finalizers) == 0 {
			*pod = nil
		}
		_ = json.NewEncoder(w).Encode(response)
		return true
	}
	var request metav1.DeleteOptions
	validDelete := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) == nil &&
		request.Preconditions != nil && request.Preconditions.UID != nil && request.Preconditions.ResourceVersion != nil &&
		*request.Preconditions.UID == actual.UID && *request.Preconditions.ResourceVersion == actual.ResourceVersion &&
		len(actual.Finalizers) == 1 && actual.Finalizers[0] == kubecontract.EnrolledRetainFinalizer
	if !validDelete {
		w.WriteHeader(409)
		return true
	}
	a.deletes.Add(1)
	now := metav1.Now()
	actual.DeletionTimestamp = &now
	version, _ := strconv.Atoi(actual.ResourceVersion)
	actual.ResourceVersion = strconv.Itoa(version + 1)
	_ = json.NewEncoder(w).Encode(actual)
	return true
}

type httpDestroyNativeWorkerSetup struct {
	Cleanup              cleanupExecutorSetup
	CA, Certificate, Key []byte
	// Configuration only. The child opens the original v3 production worker;
	// no Claim, Run, effect capability or success outcome crosses this seam.
	Detached bool
}

func httpDestroyNativeAutomatic(t *testing.T, ctx context.Context, f httpDestroyNativeContext, accepted httpDestroyNativeResponse, descriptor *state.LocalCleanupDescriptor) {
	t.Helper()
	config, err := launcherconfig.Decode(f.options.LauncherConfig)
	require.NoError(t, err)
	// Storage deployment is required by the existing worker manifest even when
	// detached cleanup never opens an archive. Only local history supplies a
	// workspace prefix; nil remains nil in the detached snapshot and Task.
	bucket, prefix := "bucket", ""
	if descriptor != nil {
		bucket, prefix = descriptor.Bucket, descriptor.Prefix
	}
	s := cleanupExecutorSetup{localReleaseNativeSetup: localReleaseNativeSetup{
		Binding: config.TrustBinding(), Identity: f.identity, Root: config.Roots[0], IssuerKey: f.options.IssuerPrivateKey,
		IssuerCertificate: f.options.IssuerCertificate, WorkspaceStorage: f.identity.StorageID, WorkspaceBucket: bucket, WorkspacePrefix: prefix,
		SandboxID: accepted.SandboxID, IntentID: f.request.IntentID, Runtime: accepted.Runtime, Generation: accepted.Generation,
		NodeUID: f.options.NodeUID, NodeBootID: f.options.NodeBootID, ObserverAddress: f.observerAddress, ProviderAddress: f.options.API.Config.Host, BudgetID: uuid.NewString(),
	}, Task: accepted.Task, LoaderConfig: f.options.LauncherConfig, LoaderKubeCA: f.options.API.Config.CAData, LoaderClockKey: f.clockKey}
	identityWire := observerNativeJSON(t, f.identity)
	profile := fmt.Sprintf(
		`{"version":1,"profile_id":"partition-worker-v1",`+
			`"namespace":%q,"restore_epoch":%q,"identity_digest":%q,`+
			`"partition_count":256,"relist_slots":4,"range_slots":20,`+
			`"range_bytes_per_second":1048576,"range_burst_bytes":262144,`+
			`"claim_slots":100,"claim_per_second":1,"claim_burst":1}`,
		accepted.Task.Namespace, accepted.Task.RestoreEpoch, hashBytes(identityWire),
	)
	_, err = httpDestroyNativePut(ctx, f.raw, accepted.Task.Namespace+"meta/worker_control_budget", profile)
	require.NoError(t, err)
	_, err = httpDestroyNativePut(ctx, f.raw, accepted.Task.Namespace+"meta/workspace_sync_recycling_budget", fmt.Sprintf(`{"version":2,"budget_id":%q,"slots_per_partition":1,"max_intent_bytes":16384}`, s.BudgetID))
	require.NoError(t, err)
	_, err = httpDestroyNativePut(ctx, f.raw, fmt.Sprintf("%sp/%02x/workspace-sync-recycling-slots/%s/00", accepted.Task.Namespace, accepted.Task.Partition, s.BudgetID), fmt.Sprintf(`{"version":2,"budget_id":%q,"partition":%d,"slot":0,"state":"free","intent_id":"","intent_digest":"","intent_birth":0,"evidence_digest":""}`, s.BudgetID, accepted.Task.Partition))
	require.NoError(t, err)
	setup := httpDestroyNativeWorkerSetup{Cleanup: s, Detached: f.detached}
	var coldHistory cleanupColdQuotaAutomaticHistory
	var retired <-chan cleanupColdQuotaAutomaticRetirement
	if f.detached {
		coldHistory = cleanupColdQuotaAutomaticBefore(t, ctx, f.raw, s)
		var stopWatch context.CancelFunc
		retired, stopWatch = cleanupColdQuotaAutomaticWatch(t, ctx, f.raw, coldHistory.pending)
		defer func() {
			stopWatch()
			for range retired {
			}
		}()
	}
	setup.CA, err = os.ReadFile(filepath.Join(f.directory, "ca.pem"))
	require.NoError(t, err)
	setup.Certificate, err = os.ReadFile(filepath.Join(f.directory, "server.pem"))
	require.NoError(t, err)
	setup.Key, err = os.ReadFile(filepath.Join(f.directory, "server-key.pem"))
	require.NoError(t, err)
	defer clear(setup.Key)
	setupPath := filepath.Join(f.directory, "http-destroy-worker.json")
	wire := observerNativeJSON(t, setup)
	defer clear(wire)
	require.NoError(t, os.WriteFile(setupPath, wire, 0600))
	defer os.Remove(setupPath)

	command := exec.Command(os.Args[0], "-test.run=^TestHTTPDestroyNativeWorkerChild$", "-test.v", "-test.timeout=100s")
	command.Env = []string{"GOMAXPROCS=2", "HTTP_DESTROY_WORKER_SETUP=" + setupPath}
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS, Setpgid: true}
	var stdout, stderr cleanupWorkerRoleOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Start())
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	joined := false
	defer func() {
		if !joined {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			t.Logf("HTTP destroy worker failure group actual Wait=%v", <-waited)
		}
		t.Logf("HTTP destroy private worker helper stdout=%s stderr=%s", stdout.String(), stderr.String())
	}()
	select {
	case err := <-waited:
		joined = true
		require.NoError(t, err, "private helper and original production worker physically joined")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Contains(t, stdout.String(), "HTTP destroy production worker PID=")
	if f.detached {
		require.Contains(t, stdout.String(), "detached cold quota verified; Pgone=true; physical worker Wait=exit0")
		select {
		case event := <-retired:
			require.NoError(t, event.err)
			require.NotNil(t, event.event)
			cleanupColdQuotaAutomaticReturned(t, ctx, f.raw, s, coldHistory, event.event)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		require.Contains(t, stdout.String(), "archive/modes verified; Pgone=true; physical worker Wait=exit0")
	}
	partition := fmt.Sprintf("%sp/%02x/", accepted.Task.Namespace, accepted.Task.Partition)
	for _, key := range []string{
		partition + "cleanup-pending/" + accepted.Task.TaskID,
		partition + "controls/" + accepted.SandboxID,
		accepted.Task.Namespace + "cleanup-execution-slots/",
	} {
		response, err := httpDestroyNativeGet(ctx, f.raw, key, clientv3.WithPrefix(), clientv3.WithLimit(33))
		require.NoError(t, err)
		require.Empty(t, response.Kvs)
	}
}

// The privileged preparation process reads its original root-only setup before
// unsharing a private /tmp, then irreversibly drops every capability and UID.
// Its files and clock never appear in the original runtime's mount namespace,
// despite the worker and user workload sharing ordinary UID/GID1000.
func TestHTTPDestroyNativeWorkerChild(t *testing.T) {
	path := os.Getenv("HTTP_DESTROY_WORKER_SETUP")
	if path == "" {
		return
	}
	assertRoot, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), assertRoot.Mode().Perm())
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	var setup httpDestroyNativeWorkerSetup
	require.NoError(t, json.Unmarshal(wire, &setup))
	// Identity deliberately excludes RestoreEpoch from its persisted JSON.
	// Like the original cleanup fixture, restore this cross-process input
	// from the same authenticated setup binding rather than rewriting config.
	setup.Cleanup.Identity.RestoreEpoch = setup.Cleanup.Binding.RestoreEpoch
	require.Equal(t, setup.Cleanup.Task.RestoreEpoch, setup.Cleanup.Identity.RestoreEpoch)
	clear(wire)
	defer clear(setup.Key)
	defer clear(setup.Cleanup.IssuerKey)
	defer clear(setup.Cleanup.LoaderClockKey)
	require.Equal(t, 0, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=16777216"))
	require.NoError(t, unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777,size=1048576"))
	for capability := uintptr(0); capability <= 40; capability++ {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESGID, 1000, 1000, 1000)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESUID, 1000, 1000, 1000)
	require.Zero(t, errno)
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, capability := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		require.Contains(t, string(status), capability+":\t0000000000000000")
	}
	require.Contains(t, string(status), "NoNewPrivs:\t1")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "private /tmp hides original root-only setup")
	dir, err := os.MkdirTemp("/tmp", "http-destroy-worker-")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	directories := []string{"spool", "target"}
	if !setup.Detached {
		directories = append(directories, "target/"+strings.TrimSuffix(setup.Cleanup.WorkspacePrefix, "/"))
	}
	for _, name := range directories {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	httpDestroyNativeWorkerRun(t, setup, dir)
}

func httpDestroyNativeWorkerRun(t *testing.T, setup httpDestroyNativeWorkerSetup, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	config, err := launcherconfig.Decode(setup.Cleanup.LoaderConfig)
	require.NoError(t, err)
	require.Equal(t, setup.Cleanup.Binding, config.TrustBinding())
	// The loader enforces the original fixed socket path. The private /run
	// mount gives UID1000 its own server there without exposing host keys or
	// changing any launcher configuration bytes or signed public claims.
	require.Equal(t, ct.ClockSocket, config.ClockSocket)
	require.NoError(t, os.Mkdir(filepath.Dir(config.ClockSocket), 0700))
	listener, err := net.Listen("unix", config.ClockSocket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(config.ClockSocket, 0600))
	clockCtx, stopClock := context.WithCancel(ctx)
	clockDone := make(chan error, 1)
	go func() {
		clockDone <- p.ServeSignedClock(clockCtx, listener, p.SignedClockServerOptions{Binding: config.TrustBinding(), Key: setup.Cleanup.LoaderClockKey, Source: nativeFixtureClock{}, MaxConnections: 4})
	}()
	defer func() {
		stopClock()
		_ = listener.Close()
		clockErr := <-clockDone
		require.True(t, clockErr == nil || errors.Is(clockErr, context.Canceled) || errors.Is(clockErr, net.ErrClosed), "%v", clockErr)
	}()
	manifest := cleanupExecutorLoaderFiles(t, setup.Cleanup, dir, setup.CA, setup.Certificate, setup.Key)
	wire, err := os.ReadFile(manifest)
	require.NoError(t, err)
	var mounted map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &mounted))
	var worker map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mounted["worker"], &worker))
	mounted["version"] = json.RawMessage("2")
	worker["automatic_cleanup"] = json.RawMessage("true")
	workerID := "http-destroy-" + uuid.NewString()
	worker["worker_id"] = observerNativeJSON(t, workerID)
	mounted["worker"] = observerNativeJSON(t, worker)
	if setup.Detached {
		mounted["version"] = json.RawMessage("3")
		var inventory state.PreparationInventoryOptions
		inventory.QuotaSlots[7] = 1 // exact original operator profile, not readiness
		mounted["preparation"] = observerNativeJSON(t, inventory)
		mounted["detached"] = observerNativeJSON(t, map[string]string{"pinned_plain_image": enrolledProfileFixtureImage})
	}
	require.NoError(t, os.WriteFile(manifest, observerNativeJSON(t, mounted), 0600))

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(setup.CA))
	certificate, err := tls.X509KeyPair(setup.Certificate, setup.Key)
	require.NoError(t, err)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}}, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer raw.Close()
	mode := "serve"
	if setup.Detached {
		mode = "serve-cold-quota" // original production v3 entry, no test bypass
	}
	command := exec.Command("/fixture/cleanup-worker", mode, manifest)
	command.Env = []string{"GOMAXPROCS=2"}
	var stdout, stderr cleanupWorkerRoleOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	require.NoError(t, command.Start())
	waited := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		waited <- command.Wait()
		close(exited)
	}()
	joined := false
	var observedLease clientv3.LeaseID
	defer func() {
		if !joined {
			_ = command.Process.Kill()
			t.Logf("HTTP destroy production worker failure cleanup Wait=%v", <-waited)
		}
		t.Logf("HTTP destroy production worker bounded stdout=%q stderr=%q", stdout.String(), stderr.String())
		if t.Failed() {
			// The failed original command has been physically joined above.
			// Diagnose retained native phases without restarting a worker,
			// resolving a Stage or exposing signed payload/private material.
			cleanupHTTPInventoryNativeFailure(t, raw, setup.Cleanup, observedLease, dir)
		}
	}()
	session := cleanupWorkerRoleSession(t, ctx, raw, setup.Cleanup.Binding.Namespace, workerID, exited)
	observedLease = clientv3.LeaseID(session.Lease)
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", command.Process.Pid))
	require.NoError(t, err)
	for _, capability := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		require.Contains(t, string(status), capability+":\t0000000000000000")
	}
	require.Contains(t, string(status), "NoNewPrivs:\t1")
	cleanupWorkerRoleSettled(t, ctx, raw, setup.Cleanup, command.Process.Pid, exited)
	pendingKey := fmt.Sprintf("%sp/%02x/cleanup-pending/%s", setup.Cleanup.Task.Namespace, setup.Cleanup.Task.Partition, setup.Cleanup.Task.TaskID)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := httpDestroyNativeGet(ctx, raw, pendingKey)
		require.NoError(t, err)
		if len(pending.Kvs) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-exited:
			t.Fatal("original automatic worker exited before normal pending sweep")
		case <-ticker.C:
		}
	}
	if setup.Detached {
		entries, err := os.ReadDir(filepath.Join(dir, "target"))
		require.NoError(t, err)
		require.Empty(t, entries, "detached cleanup must not manufacture a local archive target")
	} else {
		content, err := os.ReadFile(filepath.Join(dir, "target", setup.Cleanup.WorkspacePrefix, "nested/data"))
		require.NoError(t, err)
		require.Equal(t, "HTTP delete archive\n", string(content))
		for name, mode := range map[string]os.FileMode{"nested": 0750, "nested/data": 0640} {
			info, err := os.Stat(filepath.Join(dir, "target", setup.Cleanup.WorkspacePrefix, name))
			require.NoError(t, err)
			require.Equal(t, mode, info.Mode().Perm())
		}
	}
	require.NoError(t, command.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-waited:
		joined = true
		require.NoError(t, err, "formal automatic worker signal/Close physically joined")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Empty(t, stdout.String())
	remaining, err := httpDestroyNativeGet(ctx, raw, string(session.Key))
	require.NoError(t, err)
	require.Empty(t, remaining.Kvs)
	ttlCtx, cancelTTL := context.WithTimeout(ctx, 3*time.Second)
	ttl, err := raw.TimeToLive(ttlCtx, clientv3.LeaseID(session.Lease))
	cancelTTL()
	require.NoError(t, err)
	require.EqualValues(t, -1, ttl.TTL)
	if setup.Detached {
		t.Logf("HTTP destroy production worker PID=%d UID=%d zeroCaps=true detached cold quota verified; Pgone=true; physical worker Wait=exit0", command.Process.Pid, os.Geteuid())
	} else {
		t.Logf("HTTP destroy production worker PID=%d UID=%d zeroCaps=true archive/modes verified; Pgone=true; physical worker Wait=exit0", command.Process.Pid, os.Geteuid())
	}
}

// These test-side observations and operator setup writes have a per-RPC bound
// independent of the full physical-chain lifetime. Waiting for the original
// actor's settlement/sweep still uses its longer owner context.
func httpDestroyNativeGet(ctx context.Context, raw *clientv3.Client, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return raw.Get(bounded, key, options...)
}

func httpDestroyNativePut(ctx context.Context, raw *clientv3.Client, key, value string) (*clientv3.PutResponse, error) {
	bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return raw.Put(bounded, key, value)
}
