package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dnetwork "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
)

func TestDockerPreparationIDNaming(t *testing.T) {
	pattern := regexp.MustCompile(`^sandbox-pool-[a-z0-9]{10}$`)
	seen := make(map[string]struct{}, 128)
	for range 128 {
		id, err := newDockerPreparationID()
		require.NoError(t, err)
		assert.Regexp(t, pattern, id)
		assert.True(t, validDockerPreparationID(id))
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate preparation ID %q", id)
		}
		seen[id] = struct{}{}
	}

	legacy := "prep-00000000000000000000000000000000"
	assert.True(t, validDockerPreparationID(legacy), "legacy resources must remain recoverable")
	for _, invalid := range []string{
		"sandbox-pool-short",
		"sandbox-pool-abcdefghij-extra",
		"sandbox-pool-ABCDE12345",
		"prep-0000000000000000000000000000000",
		"prep-0000000000000000000000000000000A",
	} {
		assert.False(t, validDockerPreparationID(invalid), invalid)
	}
}

func TestDockerFUSEResourceNames(t *testing.T) {
	current := "sandbox-pool-a1b2c3d4e5"
	assert.Equal(t, "sandbox-pair-pool-a1b2c3d4e5", dockerFUSEPairNetworkName(current))
	assert.Equal(t, "sandbox-gw-pool-a1b2c3d4e5", dockerFUSEGatewayName(current))
	assert.Equal(t, "sandbox-fuse-cache-pool-a1b2c3d4e5", fuseCacheVolumeName(current))

	legacy := "prep-00000000000000000000000000000000"
	assert.Equal(t, pairNetworkPrefix+legacy, dockerFUSEPairNetworkName(legacy))
	assert.Equal(t, gatewayNamePrefix+legacy, dockerFUSEGatewayName(legacy))
	assert.Equal(t, "sandbox-fuse-cache-"+legacy, fuseCacheVolumeName(legacy))
}

func TestNewDoesNotDeleteManagedOrphanResources(t *testing.T) {
	var deletePaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == "/_ping":
			w.Header().Set("API-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/networks"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "isolated-id", "Name": isolatedNetworkName},
				{"Id": "open-id", "Name": openNetworkName},
				{"Id": "pair-id", "Name": pairNetworkPrefix + "orphan"},
			})
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/containers/json"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "gateway-id"}})
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/networks/pair-id"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id":         "pair-id",
				"Name":       pairNetworkPrefix + "orphan",
				"Containers": map[string]any{},
			})
		case req.Method == http.MethodDelete:
			deletePaths = append(deletePaths, req.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected Docker API request: "+req.Method+" "+req.URL.Path, http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	host := strings.Replace(server.URL, "http://", "tcp://", 1)
	rt, err := New(context.Background(), host, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	require.Empty(t, deletePaths, "runtime construction must not clean resources before state restoration")
}

func TestDockerPoolHitAuthorizesSameContainer(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	assert.Equal(t, info.RuntimeID, info.RuntimeUID)
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, validRuntimeAuthorization(info.RuntimeUID)))
	ready, err := rt.WaitSandboxReady(context.Background(), runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, 1)
	require.NoError(t, err)
	assert.Equal(t, info.RuntimeID, ready.RuntimeID)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, "1000:1000", fake.lastProbeUser)
	assert.Equal(t, 1, fake.commandCount["bootstrap"])
	assert.Equal(t, 1, fake.commandCount["authorize"])
	assert.Less(t, eventIndex(fake.events, "gateway-start"), eventIndex(fake.events, "runtime-start"))
	assert.Equal(t, info.RuntimeUID, fake.bootstrap.RuntimeUID)
	assert.Equal(t, "https://objects.example.com:9000", fake.bootstrap.Endpoint)
	assert.Equal(t, int64(1), fake.generation)
	assert.Equal(t, []byte("do-not-persist-access"), fake.authorize.Credentials.AccessKey)
	assert.Equal(t, []byte("do-not-persist-secret"), fake.authorize.Credentials.SecretKey)
	assert.Empty(t, fake.materializedSecrets)
	preparedJSON, err := json.Marshal(fake.containers[info.RuntimeID])
	require.NoError(t, err)
	assert.NotContains(t, string(preparedJSON), "do-not-persist-access")
	assert.NotContains(t, string(preparedJSON), "do-not-persist-secret")
	bootstrapJSON, err := json.Marshal(fake.bootstrap)
	require.NoError(t, err)
	assert.NotContains(t, string(bootstrapJSON), "access_key_file")
	assert.NotContains(t, string(bootstrapJSON), "secret_key_file")
	require.Len(t, fake.routeControls, 2)
	assert.Equal(t, []string{"/usr/sbin/ip", "route", "replace", "blackhole", "172.20.0.1/32"}, fake.routeControls[0].Cmd)
	assert.Equal(t, []string{"/usr/sbin/ip", "route", "replace", "default", "via", "172.20.0.2"}, fake.routeControls[1].Cmd)
	for _, routeControl := range fake.routeControls {
		assert.Equal(t, "root", routeControl.User)
		assert.Equal(t, "/", routeControl.WorkingDir)
		assert.False(t, routeControl.Privileged, "route control must not request Docker extended privileges")
	}
}

func TestWaitReadyPreservesDockerMounterStatusError(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	fake.mu.Lock()
	fake.healthReadyErrorCode = fuseprotocol.MounterErrorEndpointTLS
	fake.mu.Unlock()

	_, err = rt.WaitSandboxReady(context.Background(), ref, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read workspace ready status")
	assert.Contains(t, err.Error(), fuseprotocol.MounterErrorEndpointTLS)
}

func TestWaitReadyReportsOnlyDockerStatusMismatchFields(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	fake.mu.Lock()
	fake.healthReadyStatus = &fuseprotocol.MounterStatus{
		Version: 1, State: "mounting", RuntimeUID: info.RuntimeUID,
		PoolKey: strings.Repeat("a", 64), MountType: "fuse", Generation: 9,
		CacheLimitBytes: 3 << 30,
	}
	fake.mu.Unlock()

	_, err = rt.WaitSandboxReady(context.Background(), ref, 1)
	require.EqualError(t, err, "workspace ready status mismatch: state,generation,cache_limit")
	assert.NotContains(t, err.Error(), info.RuntimeUID)
	assert.NotContains(t, err.Error(), strings.Repeat("a", 64))
	assert.NotContains(t, err.Error(), "3221225472")
}

func TestDockerPrepareDerivesAndRevalidatesEndpointPolicy(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	resolvedIP := "36.170.50.43"
	rt.endpointLookup = func(context.Context, string, string) ([]net.IP, error) {
		return []net.IP{net.ParseIP(resolvedIP)}, nil
	}

	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	fake.mu.Lock()
	require.Contains(t, fake.containers, info.RuntimeID)
	assert.Equal(t, []string{"objects.example.com:36.170.50.43"}, []string(fake.containers[info.RuntimeID].host.ExtraHosts))
	fake.mu.Unlock()

	resolvedIP = "36.170.50.44"
	err = rt.PreparedSandboxHealth(context.Background(), runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, fuseDockerSpecForTest().WorkspaceFUSE.PoolKey)
	require.ErrorContains(t, err, "endpoint addresses changed")
}

func TestDockerFUSEFixedControlExecsAvoidWorkspaceWorkingDirectory(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	_, err = rt.WaitSandboxReady(context.Background(), ref, 1)
	require.NoError(t, err)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		if call.containerID != info.RuntimeID || len(call.options.Cmd) == 0 {
			continue
		}
		if call.options.Cmd[0] == fuseprotocol.MounterBinary || call.options.Cmd[0] == fuseprotocol.ProbeBinary || call.options.Cmd[0] == "/usr/sbin/ip" {
			assert.Equal(t, "/", call.options.WorkingDir, "fixed control must not chdir into a potentially blocked FUSE mount: %v", call.options.Cmd)
		}
	}
}

func TestDockerCreateSandboxRejectsFUSEBeforeMutation(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)

	_, err := rt.CreateSandbox(context.Background(), fuseDockerSpecForTest())
	require.ErrorContains(t, err, "prepare/authorize/ready")
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Empty(t, fake.events)
	assert.Empty(t, fake.containers)
	assert.Empty(t, fake.volumes)
	assert.Empty(t, fake.materializedSecrets)
}

func TestDockerQuiesceResumeTokenIsSingleUse(t *testing.T) {
	rt, _ := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	token, err := rt.QuiesceWorkspace(context.Background(), ref, 1)
	require.NoError(t, err)
	require.NoError(t, rt.FlushWorkspace(context.Background(), ref, 1))
	require.NoError(t, rt.ResumeWorkspace(context.Background(), ref, token))
	require.Error(t, rt.ResumeWorkspace(context.Background(), ref, token))
}

func TestDockerPublicExecAndFilePathsUseUID1000(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	_, err = rt.Exec(context.Background(), info.RuntimeID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	require.NoError(t, rt.ExecPipe(context.Background(), info.RuntimeID, []string{"true"}, strings.NewReader("")))
	stream, err := rt.ExecStream(context.Background(), info.RuntimeID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	for range stream {
	}
	reader, err := rt.ReadFileContent(context.Background(), info.RuntimeID, "/workspace/a")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, reader)
	require.NoError(t, reader.Close())
	require.NoError(t, rt.UploadArchive(context.Background(), info.RuntimeID, "/workspace", strings.NewReader("archive")))

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		if call.containerID == info.RuntimeID && (len(call.options.Cmd) == 0 || call.options.Cmd[0] != fuseprotocol.MounterBinary) {
			if len(call.options.Cmd) >= 3 && call.options.Cmd[0] == "/usr/sbin/ip" && call.options.Cmd[1] == "route" {
				continue
			}
			if len(call.options.Cmd) >= 3 && call.options.Cmd[0] == "sh" && strings.Contains(call.options.Cmd[2], "ip route replace") {
				continue
			}
			assert.Equal(t, dockerPublicUser, call.options.User, "public Docker exec must never inherit root: %v", call.options.Cmd)
		}
	}
	assert.Zero(t, fake.copyToCalls, "FUSE writes must not use daemon-root CopyToContainer")
	assert.Zero(t, fake.copyFromCalls, "FUSE reads must not use daemon-root CopyFromContainer")
}

func TestDockerLegacyPublicExecAndFilePathsPreserveImageUser(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.containers["legacy-runtime"] = &fakeContainer{
		config:  &container.Config{User: "root", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy"}},
		name:    "legacy",
		running: true,
	}
	fake.mu.Unlock()

	_, err := rt.Exec(context.Background(), "legacy-runtime", runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	reader, err := rt.downloadFile(context.Background(), "legacy-runtime", "/workspace/a.txt")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, reader)
	require.NoError(t, reader.Close())

	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, call := range fake.execs {
		assert.Empty(t, call.options.User, "legacy exec must inherit the image user: %v", call.options.Cmd)
	}
}

func TestDockerPublicExecAttachClosesOnContextCancellation(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.containers["legacy-runtime"] = &fakeContainer{
		config:  &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy"}},
		name:    "legacy",
		running: true,
	}
	fake.blockControlOutput = true
	fake.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := rt.Exec(ctx, "legacy-runtime", runtime.ExecRequest{Command: "true"})
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Docker public exec attach did not close after context cancellation")
	}
}

func TestDockerPreparedRemovalRetainsResourcesUntilNotFound(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	fake.inspectAfterRemoveErr = errors.New("daemon unavailable")
	err = rt.RemovePreparedSandbox(context.Background(), info.RuntimeID, info.RuntimeUID)
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	fake.mu.Lock()
	assert.Empty(t, fake.removedVolumes)
	assert.False(t, fake.pairRemoved)
	fake.mu.Unlock()

	fake.inspectAfterRemoveErr = nil
	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), info.RuntimeID, info.RuntimeUID))
	fake.mu.Lock()
	assert.Contains(t, fake.removedVolumes, fuseCacheVolumeName(preparationID))
	assert.True(t, fake.pairRemoved)
	fake.mu.Unlock()
}

func TestDockerPreparedRemovalRetriesCleanupAfterConfirmedNotFound(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)

	fake.mu.Lock()
	fake.volumeRemoveErr = errors.New("volume daemon timeout")
	fake.mu.Unlock()
	err = rt.RemovePreparedSandbox(context.Background(), info.RuntimeID, info.RuntimeUID)
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	assert.NotNil(t, rt.workspaceState(info.RuntimeID), "failed cleanup must retain its tombstone")

	fake.mu.Lock()
	fake.volumeRemoveErr = nil
	fake.mu.Unlock()
	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), info.RuntimeID, info.RuntimeUID))
	assert.Nil(t, rt.workspaceState(info.RuntimeID))
}

func TestDockerPreparedRemovalDoesNotAcceptUnknownNotFound(t *testing.T) {
	rt, _ := newFakeDockerRuntime(t)
	err := rt.RemovePreparedSandbox(context.Background(), "missing-runtime", "missing-runtime")
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
}

func TestDockerPreparedRemovalConfirmsNotFoundAfterRemoveError(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	fake.mu.Lock()
	fake.containerRemoveErr = errors.New("lost remove response")
	fake.containerRemoveDeletes = true
	fake.mu.Unlock()
	require.NoError(t, rt.RemovePreparedSandbox(context.Background(), info.RuntimeID, info.RuntimeUID))
	assert.Nil(t, rt.workspaceState(info.RuntimeID))
}

func TestDockerAuthorizationLostReplyPoisonsAttempt(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	fake.mu.Lock()
	fake.authorizeAttachErr = errors.New("lost authorize response")
	fake.mu.Unlock()
	require.Error(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	require.Error(t, rt.AuthorizeWorkspaceMount(context.Background(), ref, validRuntimeAuthorization(info.RuntimeUID)))
	fake.mu.Lock()
	assert.Equal(t, 1, fake.authorizeExecCreates, "an ambiguous authorization must never be replayed")
	fake.mu.Unlock()
}

func TestDockerRestartAdoptionRestoresImmutableSystemEgressAndCacheBytes(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	spec := fuseDockerSpecForTest()
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	spec.WorkspaceFUSE.SystemEgress.DNSCIDRs[0] = "203.0.113.53/32"
	rt.stateMu.Lock()
	rt.workspaceStates = nil
	rt.stateMu.Unlock()
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, false, nil, false))
	state := rt.workspaceState(info.RuntimeID)
	require.NotNil(t, state)
	assert.Equal(t, int64(2<<30), state.cacheBytes)
	assert.Equal(t, []string{"1.1.1.1/32"}, state.systemEgress.DNSCIDRs)
	fake.mu.Lock()
	command := fake.rootGatewayCommands[len(fake.rootGatewayCommands)-1]
	fake.mu.Unlock()
	assert.NotContains(t, command, "203.0.113.53/32")
}

func TestDockerReconcileRemovesResourceOnlyPreparationByIdentity(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	fake.mu.Lock()
	runtimeContainer := fake.containers[info.RuntimeID]
	preparationID := runtimeContainer.config.Labels["sandbox.preparation.id"]
	require.NotEmpty(t, preparationID)
	require.NotEqual(t, fuseDockerSpecForTest().ID, preparationID)
	cache := fake.volumes[fuseCacheVolumeName(preparationID)]
	assert.Equal(t, preparationID, cache.Labels["sandbox.preparation.id"])
	delete(fake.containers, info.RuntimeID)
	fake.mu.Unlock()
	rt.stateMu.Lock()
	rt.workspaceStates = nil
	rt.stateMu.Unlock()
	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), nil))
	fake.mu.Lock()
	assert.Empty(t, fake.volumes)
	assert.Empty(t, fake.removedSecrets)
	fake.mu.Unlock()
}

func TestDockerPrepareRejectsReusedCacheVolumeIdentity(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.volumeCreateWrongLabels = true
	fake.mu.Unlock()
	_, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	fake.mu.Lock()
	assert.NotContains(t, fake.events, "gateway-create")
	assert.NotEmpty(t, fake.volumes, "a volume with an untrusted identity must not be deleted")
	fake.mu.Unlock()
}

func TestDockerReconcileRemovesSecretOnlyPreparation(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.CASecretKey = "ca.crt"
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	fake.mu.Lock()
	delete(fake.containers, info.RuntimeID)
	for id, item := range fake.containers {
		if item.config.Labels["sandbox.id"] == preparationID {
			delete(fake.containers, id)
		}
	}
	delete(fake.volumes, fuseCacheVolumeName(preparationID))
	for id, item := range fake.networks {
		if item.Labels["sandbox.id"] == preparationID {
			delete(fake.networks, id)
		}
	}
	fake.mu.Unlock()
	rt.stateMu.Lock()
	rt.workspaceStates = nil
	rt.stateMu.Unlock()
	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), nil))
	fake.mu.Lock()
	assert.Contains(t, fake.removedSecrets, filepath.Join(dockerWorkspaceSecretRoot, preparationID))
	fake.mu.Unlock()
}

func TestDockerVolumeCreateAmbiguityRetainsRecoverableTombstone(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.volumeCreateErr = errors.New("lost volume create response")
	fake.volumeInspectErr = errors.New("volume inspect unavailable")
	fake.mu.Unlock()
	_, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	rt.stateMu.Lock()
	assert.Len(t, rt.workspaceStates, 1)
	rt.stateMu.Unlock()
	fake.mu.Lock()
	assert.Len(t, fake.volumes, 1)
	fake.volumeCreateErr = nil
	fake.volumeInspectErr = nil
	fake.mu.Unlock()
	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), nil))
	fake.mu.Lock()
	assert.Empty(t, fake.volumes)
	fake.mu.Unlock()
}

func TestDockerReconcileDoesNotCleanResourcesWhileRuntimeTerminationIsUnconfirmed(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	fake.mu.Lock()
	fake.inspectAfterRemoveErr = errors.New("daemon unavailable after remove")
	fake.mu.Unlock()
	err = rt.ReconcileOrphanedResources(context.Background(), nil)
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	fake.mu.Lock()
	_, volumeStillPresent := fake.volumes[fuseCacheVolumeName(preparationID)]
	assert.True(t, volumeStillPresent)
	assert.NotContains(t, fake.removedSecrets, filepath.Join(dockerWorkspaceSecretRoot, preparationID))
	fake.mu.Unlock()
}

func TestDockerReconcileProtectsRuntimeUIDs(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	first, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	secondSpec := fuseDockerSpecForTest()
	secondSpec.ID += "-two"
	second, err := rt.PrepareSandbox(context.Background(), secondSpec)
	require.NoError(t, err)
	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), map[string]struct{}{first.RuntimeUID: {}}))
	fake.mu.Lock()
	_, firstExists := fake.containers[first.RuntimeUID]
	_, secondExists := fake.containers[second.RuntimeUID]
	fake.mu.Unlock()
	assert.True(t, firstExists)
	assert.False(t, secondExists)
	assert.True(t, rt.IsStateful())
}

func TestDockerFUSEReconcileRejectsWorkspaceSecretRootChange(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	rt.secretRoot = "/srv/sandbox/workspace-secrets-a"
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.CASecretKey = "ca.crt"
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	rt.secretRoot = "/srv/sandbox/workspace-secrets-b"

	err = rt.ReconcileOrphanedResources(context.Background(), map[string]struct{}{info.RuntimeUID: {}})
	require.ErrorContains(t, err, "workspace secret root")
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Contains(t, fake.containers, info.RuntimeUID)
	assert.Empty(t, fake.removedSecrets)
}

func TestDockerFUSEReconcileIgnoresLegacyManagedResources(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	fake.mu.Lock()
	fake.containers["legacy-runtime"] = &fakeContainer{
		config:  &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy-sandbox"}},
		name:    "legacy-sandbox",
		running: true,
	}
	fake.containers["legacy-gateway"] = &fakeContainer{
		config:  &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "legacy-sandbox", "sandbox.role": "gateway"}},
		name:    "sandbox-gw-legacy-sandbox",
		running: true,
	}
	fake.mu.Unlock()

	require.NoError(t, rt.ReconcileOrphanedResources(context.Background(), map[string]struct{}{"legacy-runtime": {}}))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Contains(t, fake.containers, "legacy-runtime")
	assert.Contains(t, fake.containers, "legacy-gateway")
}

func TestDockerFUSEGatewayUsesRoutablePairAndExactSystemEgress(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, preparationID, fake.containers[info.RuntimeID].name)
	pair, exists := fake.networkCreateOptions[dockerFUSEPairNetworkName(preparationID)]
	require.True(t, exists)
	assert.False(t, pair.Internal, "Docker internal bridges drop transit packets before they reach the policy gateway")
	assert.Contains(t, fake.volumes, fuseCacheVolumeName(preparationID))
	var gatewayName string
	for _, item := range fake.containers {
		if item.config.Labels["sandbox.role"] == "gateway" && item.config.Labels["sandbox.id"] == preparationID {
			gatewayName = item.name
			break
		}
	}
	assert.Equal(t, dockerFUSEGatewayName(preparationID), gatewayName)
	command := strings.Join(fake.rootGatewayCommands, "\n")
	assert.Contains(t, command, "-d 1.1.1.1/32 -p udp --dport 53 -j ACCEPT")
	assert.Contains(t, command, "-d 198.51.100.10/32 -p tcp --dport 9000 -j ACCEPT")
	assert.NotContains(t, command, "iptables -A FORWARD -p udp --dport 53", "DNS must always have an exact destination")
}

func TestDockerFUSERejectsCiliumModeWithoutMutatingDocker(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.SystemEgress.Mode = runtime.SystemEgressCiliumFQDN
	_, err := rt.PrepareSandbox(context.Background(), spec)
	require.Error(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Empty(t, fake.events)
	assert.Empty(t, fake.volumes)
}

func TestDockerFUSERequiresTrustedSecretMaterializerBeforeMutation(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	rt.secretMaterializer = nil
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.CASecretKey = "ca.crt"
	_, err := rt.PrepareSandbox(context.Background(), spec)
	require.Error(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Empty(t, fake.events)
	assert.Empty(t, fake.volumes)
}

func TestDockerFUSEMaterializesExactRootOnlySecretDirectory(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.CASecretKey = "ca.crt"
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.materializedSecrets, 1)
	assert.Equal(t, filepath.Join(dockerWorkspaceSecretRoot, preparationID), fake.materializedSecrets[0])
}

func TestDockerFUSEUsesConfiguredSecretRootForMaterializationAndContainerBind(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	rt.secretRoot = "/srv/sandbox/workspace-secrets"
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.CASecretKey = "ca.crt"
	info, err := rt.PrepareSandbox(context.Background(), spec)
	require.NoError(t, err)
	preparationID := rt.workspaceState(info.RuntimeID).preparationID
	expectedSource := filepath.Join(rt.secretRoot, preparationID)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, []string{expectedSource}, fake.materializedSecrets)
	require.NotNil(t, fake.containers[info.RuntimeID])
	assert.Contains(t, fake.containers[info.RuntimeID].host.Binds, expectedSource+":"+dockerMounterSecretPath+":ro")
}

func TestDockerFUSEUserNetworkUpdateRetainsSystemRules(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	info, err := rt.PrepareSandbox(context.Background(), fuseDockerSpecForTest())
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}
	require.NoError(t, rt.UpdateFUSENetwork(context.Background(), ref, true, []string{"203.0.113.7/32"}, false))
	fake.mu.Lock()
	initial := fake.rootGatewayCommands[0]
	command := fake.rootGatewayCommands[len(fake.rootGatewayCommands)-1]
	fake.mu.Unlock()
	assert.Contains(t, initial, "-d 1.1.1.1/32 -p udp --dport 53 -j ACCEPT")
	assert.Contains(t, initial, "-d 198.51.100.10/32 -p tcp --dport 9000 -j ACCEPT")
	assert.NotContains(t, command, "SBOX_SYSTEM")
	assert.NotContains(t, command, "-F FORWARD")
	assert.Contains(t, command, "-d 203.0.113.7/32 -j ACCEPT")
}

func validRuntimeAuthorization(uid string) runtime.WorkspaceMountAuthorization {
	return runtime.WorkspaceMountAuthorization{
		RuntimeUID: uid, PoolKey: strings.Repeat("a", 64), WorkspaceHash: strings.Repeat("b", 64),
		Prefix: "users/a/workspaces/b/", LeaseGeneration: 1, MountAttempt: 1,
	}
}

func eventIndex(events []string, target string) int {
	for index, event := range events {
		if event == target {
			return index
		}
	}
	return len(events) + 1
}

type fakeContainer struct {
	config   *container.Config
	host     *container.HostConfig
	name     string
	running  bool
	networks map[string]*dnetwork.EndpointSettings
}

type fakeExec struct {
	containerID string
	options     container.ExecOptions
}

type fakeDockerAPI struct {
	mu                       sync.Mutex
	containers               map[string]*fakeContainer
	networks                 map[string]dnetwork.Inspect
	execs                    map[string]fakeExec
	volumes                  map[string]volume.Volume
	nextContainer            int
	nextExec                 int
	events                   []string
	commandCount             map[string]int
	lastProbeUser            string
	bootstrap                fuseprotocol.BootstrapConfig
	generation               int64
	removedVolumes           []string
	pairRemoved              bool
	inspectAfterRemoveErr    error
	containerRemoveRequested map[string]bool
	copyToCalls              int
	copyFromCalls            int
	lastCopyToUID            int
	lastCopyToGID            int
	lastCopyToOptions        container.CopyToContainerOptions
	networkCreateOptions     map[string]dnetwork.CreateOptions
	rootGatewayCommands      []string
	routeControls            []container.ExecOptions
	materializedSecrets      []string
	removedSecrets           []string
	volumeRemoveErr          error
	volumeCreateWrongLabels  bool
	volumeCreateErr          error
	volumeInspectErr         error
	containerRemoveErr       error
	containerRemoveDeletes   bool
	authorizeAttachErr       error
	authorizeExecCreates     int
	authorize                fuseprotocol.AuthorizeRequest
	blockControlOutput       bool
	removeExecExitCode       int
	removeExecInspectErr     error
	healthReadyErrorCode     string
	healthReadyStatus        *fuseprotocol.MounterStatus
}

func newFakeDockerRuntime(t *testing.T) (*Runtime, *fakeDockerAPI) {
	t.Helper()
	fake := &fakeDockerAPI{
		containers: make(map[string]*fakeContainer), networks: make(map[string]dnetwork.Inspect), execs: make(map[string]fakeExec),
		volumes: make(map[string]volume.Volume), commandCount: make(map[string]int), containerRemoveRequested: make(map[string]bool),
		networkCreateOptions: make(map[string]dnetwork.CreateOptions),
	}
	fake.networks["isolated-id"] = dnetwork.Inspect{ID: "isolated-id", Name: isolatedNetworkName}
	fake.networks["open-id"] = dnetwork.Inspect{ID: "open-id", Name: openNetworkName}
	return &Runtime{
		cli: fake, isolatedNetworkID: "isolated-id", openNetworkID: "open-id", gatewayImage: "gateway@sha256:" + strings.Repeat("3", 64),
		secretMaterializer: fake, secretValidator: func(string, string) error { return nil },
		fuseCredentials: runtime.FUSECredentials{AccessKey: []byte("do-not-persist-access"), SecretKey: []byte("do-not-persist-secret")},
	}, fake
}

func TestListSandboxesReturnsRuntimeLabels(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	labels := map[string]string{
		"sandbox.id":             "sandbox-pool-fuse",
		"sandbox.pool":           "true",
		"sandbox.workspace.mode": "fuse",
	}
	fake.containers["fuse-runtime"] = &fakeContainer{
		config:  &container.Config{Labels: labels},
		name:    "sandbox-pool-fuse",
		running: true,
	}

	infos, err := rt.ListSandboxes(context.Background(), map[string]string{"sandbox.pool": "true"})
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, labels, infos[0].Labels)
}

func (f *fakeDockerAPI) Materialize(_ context.Context, _ runtime.SandboxSpec, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.materializedSecrets = append(f.materializedSecrets, target)
	return nil
}

func (f *fakeDockerAPI) RemoveSecret(_ context.Context, target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedSecrets = append(f.removedSecrets, target)
	return nil
}

func (f *fakeDockerAPI) ListSecretPreparations(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	removed := make(map[string]struct{}, len(f.removedSecrets))
	for _, target := range f.removedSecrets {
		removed[target] = struct{}{}
	}
	var result []string
	for _, target := range f.materializedSecrets {
		if _, wasRemoved := removed[target]; wasRemoved {
			continue
		}
		result = append(result, filepath.Base(target))
	}
	return result, nil
}

func (f *fakeDockerAPI) Close() error { return nil }

func (f *fakeDockerAPI) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig, networking *dnetwork.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextContainer++
	id := fmt.Sprintf("container-%d", f.nextContainer)
	attached := make(map[string]*dnetwork.EndpointSettings)
	if networking != nil {
		for networkName := range networking.EndpointsConfig {
			attached[networkName] = &dnetwork.EndpointSettings{IPAddress: "172.20.0.2"}
		}
	}
	f.containers[id] = &fakeContainer{config: cfg, host: host, name: name, networks: attached}
	if cfg.Labels["sandbox.role"] == "gateway" {
		f.events = append(f.events, "gateway-create")
	} else {
		f.events = append(f.events, "runtime-create")
	}
	return container.CreateResponse{ID: id}, nil
}

func (f *fakeDockerAPI) ContainerStart(_ context.Context, id string, _ container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.containers[id]
	if c == nil {
		return errdefs.NotFound(errors.New("not found"))
	}
	c.running = true
	if c.config.Labels["sandbox.role"] == "gateway" {
		f.events = append(f.events, "gateway-start")
	} else {
		f.events = append(f.events, "runtime-start")
	}
	return nil
}

func (f *fakeDockerAPI) ContainerStop(context.Context, string, container.StopOptions) error {
	return nil
}

func (f *fakeDockerAPI) ContainerRemove(_ context.Context, id string, _ container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[id]; !ok {
		return errdefs.NotFound(errors.New("not found"))
	}
	f.containerRemoveRequested[id] = true
	if f.inspectAfterRemoveErr == nil || f.containerRemoveDeletes {
		delete(f.containers, id)
	}
	return f.containerRemoveErr
}

func (f *fakeDockerAPI) ContainerInspect(_ context.Context, id string) (types.ContainerJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containerRemoveRequested[id] && f.inspectAfterRemoveErr != nil {
		return types.ContainerJSON{}, f.inspectAfterRemoveErr
	}
	c := f.containers[id]
	if c == nil {
		return types.ContainerJSON{}, errdefs.NotFound(errors.New("not found"))
	}
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{ID: id, Name: "/" + c.name, Created: time.Now().Format(time.RFC3339Nano), State: &types.ContainerState{Running: c.running}},
		Config:            c.config, NetworkSettings: &types.NetworkSettings{Networks: c.networks},
	}, nil
}

func (f *fakeDockerAPI) ContainerList(_ context.Context, opts container.ListOptions) ([]types.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []types.Container
	for id, c := range f.containers {
		matches := true
		for _, requirement := range opts.Filters.Get("label") {
			key, value, found := strings.Cut(requirement, "=")
			if !found || c.config.Labels[key] != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		result = append(result, types.Container{ID: id, Labels: c.config.Labels, State: map[bool]string{true: "running", false: "exited"}[c.running]})
	}
	return result, nil
}

func (f *fakeDockerAPI) ContainerRename(context.Context, string, string) error { return nil }

func (f *fakeDockerAPI) ContainerExecCreate(_ context.Context, containerID string, options container.ExecOptions) (types.IDResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextExec++
	id := fmt.Sprintf("exec-%d", f.nextExec)
	f.execs[id] = fakeExec{containerID: containerID, options: options}
	if len(options.Cmd) >= 2 && options.Cmd[0] == fuseprotocol.MounterBinary && options.Cmd[1] == "authorize" {
		f.authorizeExecCreates++
	}
	if len(options.Cmd) >= 3 && options.Cmd[0] == "/usr/sbin/ip" && options.Cmd[1] == "route" {
		f.routeControls = append(f.routeControls, options)
	}
	return types.IDResponse{ID: id}, nil
}

func (f *fakeDockerAPI) ContainerExecStart(_ context.Context, execID string, _ container.ExecStartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Gateway and route setup use start-without-attach; capture only the exact
	// exec being started.
	exec := f.execs[execID]
	if exec.options.User == "root" && len(exec.options.Cmd) == 3 && exec.options.Cmd[0] == "sh" && exec.options.Cmd[1] == "-c" {
		if c := f.containers[exec.containerID]; c != nil && c.config.Labels["sandbox.role"] == "gateway" {
			f.rootGatewayCommands = append(f.rootGatewayCommands, exec.options.Cmd[2])
		}
	}
	return nil
}

func (f *fakeDockerAPI) ContainerExecAttach(_ context.Context, execID string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	f.mu.Lock()
	exec, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return types.HijackedResponse{}, errors.New("unknown exec")
	}
	if len(exec.options.Cmd) >= 2 && exec.options.Cmd[0] == fuseprotocol.MounterBinary && exec.options.Cmd[1] == "authorize" {
		f.mu.Lock()
		err := f.authorizeAttachErr
		f.mu.Unlock()
		if err != nil {
			return types.HijackedResponse{}, err
		}
	}
	conn := newFakeHijackConn(func(input []byte) []byte { return f.execOutput(exec, input) })
	f.mu.Lock()
	conn.blockResponse = f.blockControlOutput
	f.mu.Unlock()
	if !exec.options.AttachStdin {
		_ = conn.CloseWrite()
	}
	return types.HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn)}, nil
}

func (f *fakeDockerAPI) execOutput(exec fakeExec, input []byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := exec.options.Cmd
	var output any = map[string]any{}
	if len(argv) >= 2 && argv[0] == fuseprotocol.MounterBinary {
		command := argv[1]
		if command == "health" {
			command += "-" + argv[2]
		}
		f.commandCount[command]++
		switch command {
		case "bootstrap":
			_ = fuseprotocol.Decode(input, &f.bootstrap)
			output = fuseprotocol.ControlAck{Version: 1, Accepted: true, RuntimeUID: f.bootstrap.RuntimeUID}
		case "authorize":
			var auth fuseprotocol.AuthorizeRequest
			_ = fuseprotocol.DecodeExact(input, &auth)
			f.authorize = auth
			f.generation = auth.LeaseGeneration
			output = fuseprotocol.ControlAck{Version: 1, Accepted: true, RuntimeUID: auth.RuntimeUID, Generation: auth.LeaseGeneration}
		case "health-prepared":
			output = fuseprotocol.MounterStatus{Version: 1, State: "prepared", RuntimeUID: exec.containerID, PoolKey: strings.Repeat("a", 64), CacheLimitBytes: 2 << 30}
		case "health-ready":
			if f.healthReadyErrorCode != "" {
				raw := []byte("workspace-mounter-error:" + f.healthReadyErrorCode + "\n")
				return append([]byte{2, 0, 0, 0, byte(len(raw) >> 24), byte(len(raw) >> 16), byte(len(raw) >> 8), byte(len(raw))}, raw...)
			}
			if f.healthReadyStatus != nil {
				output = *f.healthReadyStatus
			} else {
				output = fuseprotocol.MounterStatus{Version: 1, State: "ready", RuntimeUID: exec.containerID, PoolKey: strings.Repeat("a", 64), MountType: "fuse", Generation: f.generation, CacheLimitBytes: 2 << 30}
			}
		case "flush":
			output = fuseprotocol.ControlAck{Version: 1, Accepted: true, RuntimeUID: exec.containerID, Generation: f.generation}
		case "shutdown":
			output = fuseprotocol.ShutdownAck{Version: 1, RuntimeUID: exec.containerID, Generation: f.generation, GracefulUnmount: true}
		}
	} else if len(argv) >= 2 && argv[0] == fuseprotocol.ProbeBinary {
		f.lastProbeUser = exec.options.User
		generation := f.generation
		token := ""
		if argv[1] == "quiesce" {
			token = "p1.test-token"
		}
		output = fuseprotocol.ProbeStatus{Version: 1, RuntimeUID: exec.containerID, Generation: generation, OK: true, Token: token}
	}
	raw, _ := json.Marshal(output)
	var framed bytes.Buffer
	_, _ = framed.Write([]byte{1, 0, 0, 0, byte(len(raw) >> 24), byte(len(raw) >> 16), byte(len(raw) >> 8), byte(len(raw))})
	_, _ = framed.Write(raw)
	return framed.Bytes()
}

func (f *fakeDockerAPI) ContainerExecInspect(_ context.Context, execID string) (container.ExecInspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	exec := f.execs[execID]
	if len(exec.options.Cmd) == 3 && exec.options.Cmd[0] == fuseprotocol.MounterBinary && exec.options.Cmd[1] == "health" && exec.options.Cmd[2] == "ready" && f.healthReadyErrorCode != "" {
		return container.ExecInspect{ExitCode: 1}, nil
	}
	if len(exec.options.Cmd) >= 3 && exec.options.Cmd[0] == "sh" && exec.options.Cmd[1] == "-c" && strings.Contains(exec.options.Cmd[2], "rm -f --") {
		if f.removeExecInspectErr != nil {
			return container.ExecInspect{}, f.removeExecInspectErr
		}
		return container.ExecInspect{ExitCode: f.removeExecExitCode}, nil
	}
	return container.ExecInspect{ExitCode: 0}, nil
}
func (f *fakeDockerAPI) CopyFromContainer(context.Context, string, string) (io.ReadCloser, container.PathStat, error) {
	f.mu.Lock()
	f.copyFromCalls++
	f.mu.Unlock()
	return io.NopCloser(bytes.NewReader(nil)), container.PathStat{}, nil
}
func (f *fakeDockerAPI) CopyToContainer(_ context.Context, _ string, _ string, reader io.Reader, options container.CopyToContainerOptions) error {
	tarReader := tar.NewReader(reader)
	header, err := tarReader.Next()
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, tarReader); err != nil {
		return err
	}
	for {
		_, err = tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, tarReader); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.copyToCalls++
	f.lastCopyToUID = header.Uid
	f.lastCopyToGID = header.Gid
	f.lastCopyToOptions = options
	f.mu.Unlock()
	return nil
}

func (f *fakeDockerAPI) NetworkCreate(_ context.Context, name string, opts dnetwork.CreateOptions) (dnetwork.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "network-" + name
	f.networks[id] = dnetwork.Inspect{
		ID: id, Name: name, Labels: opts.Labels,
		IPAM: dnetwork.IPAM{Config: []dnetwork.IPAMConfig{{Subnet: "172.20.0.0/16", Gateway: "172.20.0.1"}}},
	}
	f.networkCreateOptions[name] = opts
	return dnetwork.CreateResponse{ID: id}, nil
}

func (f *fakeDockerAPI) NetworkList(_ context.Context, opts dnetwork.ListOptions) ([]dnetwork.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]dnetwork.Summary, 0, len(f.networks))
	for _, item := range f.networks {
		matches := true
		for _, requirement := range opts.Filters.Get("label") {
			key, value, found := strings.Cut(requirement, "=")
			if !found || item.Labels[key] != value {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		result = append(result, dnetwork.Summary{ID: item.ID, Name: item.Name, Labels: item.Labels})
	}
	return result, nil
}

func (f *fakeDockerAPI) NetworkInspect(_ context.Context, id string, _ dnetwork.InspectOptions) (dnetwork.Inspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if item, ok := f.networks[id]; ok {
		return item, nil
	}
	for _, item := range f.networks {
		if item.Name == id {
			return item, nil
		}
	}
	return dnetwork.Inspect{}, errdefs.NotFound(errors.New("not found"))
}
func (f *fakeDockerAPI) NetworkConnect(context.Context, string, string, *dnetwork.EndpointSettings) error {
	return nil
}
func (f *fakeDockerAPI) NetworkDisconnect(context.Context, string, string, bool) error { return nil }
func (f *fakeDockerAPI) NetworkRemove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, item := range f.networks {
		if key == id || item.Name == id {
			delete(f.networks, key)
			if strings.HasPrefix(item.Name, pairNetworkPrefix) {
				f.pairRemoved = true
			}
			return nil
		}
	}
	return errdefs.NotFound(errors.New("not found"))
}
func (f *fakeDockerAPI) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	item := volume.Volume{Name: opts.Name, Labels: opts.Labels}
	if f.volumeCreateWrongLabels {
		item.Labels = cloneLabels(opts.Labels)
		item.Labels[dockerPreparationLabel] = "prep-00000000000000000000000000000000"
	}
	f.volumes[opts.Name] = item
	return item, f.volumeCreateErr
}
func (f *fakeDockerAPI) VolumeInspect(_ context.Context, name string) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumeInspectErr != nil {
		return volume.Volume{}, f.volumeInspectErr
	}
	item, ok := f.volumes[name]
	if !ok {
		return volume.Volume{}, errdefs.NotFound(errors.New("not found"))
	}
	return item, nil
}
func (f *fakeDockerAPI) VolumeList(_ context.Context, opts volume.ListOptions) (volume.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := volume.ListResponse{}
	for _, item := range f.volumes {
		matches := true
		for _, requirement := range opts.Filters.Get("label") {
			key, value, found := strings.Cut(requirement, "=")
			if !found || item.Labels[key] != value {
				matches = false
				break
			}
		}
		if matches {
			copy := item
			result.Volumes = append(result.Volumes, &copy)
		}
	}
	return result, nil
}
func (f *fakeDockerAPI) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.volumeRemoveErr != nil {
		return f.volumeRemoveErr
	}
	delete(f.volumes, name)
	f.removedVolumes = append(f.removedVolumes, name)
	return nil
}

type fakeHijackConn struct {
	mu            sync.Mutex
	input         bytes.Buffer
	response      *bytes.Reader
	ready         chan struct{}
	closed        bool
	respond       func([]byte) []byte
	blockResponse bool
}

func newFakeHijackConn(respond func([]byte) []byte) *fakeHijackConn {
	return &fakeHijackConn{ready: make(chan struct{}), respond: respond}
}
func (c *fakeHijackConn) Read(p []byte) (int, error) {
	<-c.ready
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.response.Read(p)
}
func (c *fakeHijackConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.input.Write(p)
}
func (c *fakeHijackConn) CloseWrite() error {
	c.mu.Lock()
	if c.response == nil && !c.blockResponse {
		c.response = bytes.NewReader(c.respond(append([]byte(nil), c.input.Bytes()...)))
		close(c.ready)
	}
	c.mu.Unlock()
	return nil
}
func (c *fakeHijackConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.response == nil {
			c.response = bytes.NewReader(nil)
			close(c.ready)
		}
	}
	c.mu.Unlock()
	return nil
}
func (c *fakeHijackConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (c *fakeHijackConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (c *fakeHijackConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeHijackConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeHijackConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return string(a) }
func (a fakeAddr) String() string  { return string(a) }
