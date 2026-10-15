//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestLocalRuntimeReleaseNativeEffects(t *testing.T) {
	for i := 0; i < 7; i++ {
		mode := "http-" + strconv.Itoa(i)
		if !t.Run(mode, func(t *testing.T) { runLocalRuntimeReleaseNative(t, mode) }) {
			return
		}
	}
}

func TestLocalRuntimeReleaseNativeClose(t *testing.T) { runLocalRuntimeReleaseNative(t, "close-http") }

func localReleaseCloseHTTP(t *testing.T, ctx context.Context, b *state.Backend, claim *state.TaskClaim, destination *ct.TerminationDestination, address string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	control := func(path string) {
		r, err := client.Get(address + path)
		require.NoError(t, err)
		_, readErr := io.Copy(io.Discard, r.Body)
		closeErr := r.Body.Close()
		require.NoError(t, readErr)
		require.NoError(t, closeErr)
		require.Equal(t, http.StatusOK, r.StatusCode)
	}
	done := make(chan error, 1)
	go func() { _, err := b.TerminateTaskRuntimeLocal(ctx, claim, destination); done <- err }()
	control("/fixture/wait-blocked")
	select {
	case err := <-done:
		t.Fatalf("original Terminate returned before Close: %v", err)
	default:
	}
	require.NoError(t, b.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("original Terminate did not join after Backend.Close")
	}
	control("/fixture/wait-canceled")
	// The remote fixture handler deliberately remains alive after cancellation;
	// it is not part of the client's IO ownership. Release and join it separately.
	control("/fixture/release-handler")
	_, err := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	require.Error(t, err)
	t.Log("Backend.Close canceled real blocked HTTP, original Terminate joined, closed backend refused release owner admission; remote handler separately released and joined")
}

// Four distinct exact owned objects, including both Cilium slots. This uses
// the real provider's ownership validator and never calls its private helpers.
func localReleaseNativePolicies(logical, runtime, runtimeUID string) map[string]*unstructured.Unstructured {
	objects := map[string]*unstructured.Unstructured{}
	for _, id := range []string{logical, runtime} {
		for _, cilium := range []bool{false, true} {
			kind, api, prefix, path, role, selector := "NetworkPolicy", "networking.k8s.io/v1", "sandbox-", "/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/", "ordinary", "podSelector"
			if cilium {
				kind, api, prefix, path, role, selector = "CiliumNetworkPolicy", "cilium.io/v2", "sandbox-private-deny-", "/apis/cilium.io/v2/namespaces/runtimes/ciliumnetworkpolicies/", "ordinary-private-deny", "endpointSelector"
			}
			name := prefix + id
			objects[path+name] = &unstructured.Unstructured{Object: map[string]any{"apiVersion": api, "kind": kind, "metadata": map[string]any{"name": name, "namespace": "runtimes", "uid": uuid.NewString(), "resourceVersion": "10", "labels": map[string]any{"sandbox.managed": "true", "sandbox.id": id, "sandbox.policy.role": role, "sandbox.runtime.id": runtime}, "annotations": map[string]any{"sandbox.runtime.uid": runtimeUID}}, "spec": map[string]any{selector: map[string]any{"matchLabels": map[string]any{"sandbox.id": id}}}}}
		}
	}
	return objects
}

func localReleaseDropHTTP(w http.ResponseWriter) error {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return err
	}
	return conn.Close()
}

func localReleaseUnknownEffect(t *testing.T, ctx context.Context, b *state.Backend, claim *state.TaskClaim, destination *ct.TerminationDestination, s localReleaseNativeSetup, tlsConfig *tls.Config, ns state.Namespace, task state.TaskReference) {
	t.Helper()
	result, err := b.TerminateTaskRuntimeLocal(ctx, claim, destination)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, result.Outcome)
	original, err := b.LoadTaskRuntimeTermination(ctx, task)
	require.NoError(t, err)
	repeated, err := b.TerminateTaskRuntimeLocal(ctx, claim, destination)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeUnknown, repeated.Outcome)
	require.Equal(t, original, repeated.Entry)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{s.EtcdAddress}, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	key, err := ns.Key("p", fmt.Sprintf("%02x", task.Partition), "tasks", task.TaskID, "runtime-provider-cleanup")
	require.NoError(t, err)
	before, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, before.Kvs, 1)
	var record struct{ Slots []struct{ State string } }
	require.NoError(t, json.Unmarshal(before.Kvs[0].Value, &record))
	index, err := strconv.Atoi(strings.TrimPrefix(s.Fault, "http-"))
	if s.Fault == "ack-provider-intent" {
		index, err = 0, nil
	}
	require.NoError(t, err)
	require.Equal(t, "intent", record.Slots[index].State, "lost ACK leaves original durable intent; it never grants resend")
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	claim, err = b.ClaimTask(ctx, task, "effect-unknown-fresh", time.Minute)
	require.NoError(t, err)
	repeated, err = b.TerminateTaskRuntimeLocal(ctx, claim, destination)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, repeated.Outcome)
	after, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs, "fresh claim cannot rewrite unknown owner or grant resend")
	retained, err := b.LoadTaskRuntimeTermination(ctx, task)
	require.NoError(t, err)
	require.Equal(t, original, retained, "historical Current remains the original sender")
	release, err := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, release.Outcome)
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	t.Logf("actual HTTP slot=%d ACK loss: original+fresh attempts preserve durable unknown and historical sender; no release", index)
}
