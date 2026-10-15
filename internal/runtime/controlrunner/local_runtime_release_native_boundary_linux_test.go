//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestLocalRuntimeReleaseNativeBoundaries(t *testing.T) {
	for _, suffix := range []string{"runtime", "node", "claim", "restore", "complete", "provider-uid", "provider-rv"} {
		mode := "boundary-" + suffix
		if !t.Run(mode, func(t *testing.T) { runLocalRuntimeReleaseNative(t, mode) }) {
			return
		}
	}
}

func TestLocalRuntimeReleaseNativeRecoveryACKs(t *testing.T) {
	for _, mode := range []string{"ack-provider-intent", "ack-terminated"} {
		if !t.Run(mode, func(t *testing.T) { runLocalRuntimeReleaseNative(t, mode) }) {
			return
		}
	}
}

func localReleaseBoundaryRefusal(t *testing.T, ctx context.Context, b *state.Backend, claim *state.TaskClaim, destination *ct.TerminationDestination, s localReleaseNativeSetup, config *tls.Config, ns state.Namespace, workspace state.WorkspaceIdentity, task state.TaskReference) {
	t.Helper()
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: config, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	partition := fmt.Sprintf("%02x", task.Partition)
	var key, replace string
	switch s.Fault {
	case "boundary-runtime":
		key, err = ns.Key("p", partition, "controls", s.SandboxID)
		replace = s.Runtime.UID
	case "boundary-node":
		key, err = ns.Key("p", partition, "intents", s.IntentID, "runtime-enrollment")
		replace = s.NodeUID
	case "boundary-complete":
		key, err = ns.Key("p", partition, "tasks", task.TaskID, "workspace-sync-local-complete")
	case "boundary-restore":
		key = ns.Root() + "meta/restore_epoch"
	case "boundary-claim":
		_, err = raw.Revoke(ctx, clientv3.LeaseID(claim.Reference().LeaseID))
	case "boundary-provider-uid", "boundary-provider-rv":
		client := &http.Client{Timeout: 2 * time.Second}
		response, e := client.Get(s.ProviderAddress + "/fixture/arm-boundary")
		require.NoError(t, e)
		require.Equal(t, 200, response.StatusCode)
		require.NoError(t, response.Body.Close())
		client.CloseIdleConnections()
	default:
		t.Fatal("unknown bounded native refusal")
	}
	require.NoError(t, err)
	if key != "" {
		old, err := raw.Get(ctx, key)
		require.NoError(t, err)
		require.Len(t, old.Kvs, 1)
		value := string(old.Kvs[0].Value)
		if replace != "" {
			require.Contains(t, value, replace)
			value = strings.ReplaceAll(value, replace, uuid.NewString())
		}
		if s.Fault == "boundary-restore" {
			value = "different-restore"
		}
		_, err = raw.Put(ctx, key, value)
		require.NoError(t, err)
	}
	result, err := b.TerminateTaskRuntimeLocal(ctx, claim, destination)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, result.Outcome)
	release, err := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, release.Outcome)
	for _, suffix := range []string{"runtime-termination", "owner-release"} {
		key, err := ns.Key("p", partition, "tasks", task.TaskID, suffix)
		require.NoError(t, err)
		point, err := raw.Get(ctx, key)
		require.NoError(t, err)
		require.Empty(t, point.Kvs)
	}
	ownerKey, err := ns.Key("p", partition, "workspaces", workspace.Hash(), "owner")
	require.NoError(t, err)
	owner, err := raw.Get(ctx, ownerKey)
	require.NoError(t, err)
	require.Len(t, owner.Kvs, 1)
	t.Logf("actual native authority refusal %s: termination/release absent, original owner retained", s.Fault)
}
