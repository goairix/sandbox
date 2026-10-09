package kubernetes

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type controlKeyStore struct {
	state.Store
	mu       sync.Mutex
	values   map[string][]byte
	writeErr error
}

func (s *controlKeyStore) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return false, s.writeErr
	}
	if ttl != 0 {
		return false, errors.New("authority must not expire")
	}
	if s.values[key] != nil {
		return false, nil
	}
	s.values[key] = append([]byte(nil), value...)
	return true, nil
}
func (s *controlKeyStore) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.values[key]...), nil
}
func TestWorkspaceControlAuthoritySurvivesReplicaRestart(t *testing.T) {
	store := &controlKeyStore{values: map[string][]byte{}}
	keys := make(chan ed25519.PrivateKey, 8)
	keyErrors := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := LoadWorkspaceControlKey(context.Background(), store, "scope-a")
			keyErrors <- err
			keys <- key
		}()
	}
	wg.Wait()
	close(keys)
	close(keyErrors)
	for err := range keyErrors {
		require.NoError(t, err)
	}
	first := <-keys
	for key := range keys {
		require.Equal(t, first, key)
	}
	restarted, err := LoadWorkspaceControlKey(context.Background(), store, "scope-a")
	require.NoError(t, err)
	require.Equal(t, first, restarted)
	other, err := LoadWorkspaceControlKey(context.Background(), store, "scope-b")
	require.NoError(t, err)
	require.NotEqual(t, first, other)
	require.Len(t, store.values, 2)
}
func TestWorkspaceControlAuthorityRejectsUnconfirmedOrCorruptState(t *testing.T) {
	store := &controlKeyStore{values: map[string][]byte{}}
	_, err := LoadWorkspaceControlKey(context.Background(), store, "scope-a")
	require.NoError(t, err)
	store.writeErr = state.ErrDurabilityUnconfirmed
	key, err := LoadWorkspaceControlKey(context.Background(), store, "scope-a")
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.Nil(t, key)
	store.writeErr = nil
	for name := range store.values {
		store.values[name] = []byte("corrupt")
	}
	key, err = LoadWorkspaceControlKey(context.Background(), store, "scope-a")
	require.Error(t, err)
	require.Nil(t, key)
	for _, value := range store.values {
		require.Equal(t, []byte("corrupt"), value)
	}
}
func TestPreparedPodContainsOnlyPublicControlAuthority(t *testing.T) {
	rt, client := newFakeKubernetesRuntime(t, preparedScript())
	prepared, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	pod, err := client.CoreV1().Pods(rt.namespace).Get(context.Background(), prepared.RuntimeID, metav1.GetOptions{})
	require.NoError(t, err)
	public := rt.workspaceControlKey.Public().(ed25519.PublicKey)
	found := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == fuseprotocol.ProbeControlPublicKeyEnv {
			found = true
			require.Equal(t, base64.RawURLEncoding.EncodeToString(public), env.Value)
		}
	}
	require.True(t, found)
	raw, err := json.Marshal(pod)
	require.NoError(t, err)
	require.NotContains(t, string(raw), base64.RawURLEncoding.EncodeToString(rt.workspaceControlKey.Seed()))
}
func TestKubernetesCleanupSendsPrivateSignedStdin(t *testing.T) {
	script := preparedScript()
	rt, _ := newFakeKubernetesRuntime(t, script)
	prepared, err := rt.PrepareSandbox(context.Background(), preparedFUSESpecForTest())
	require.NoError(t, err)
	expectedArgv := []string{workspaceProbeBinary, "quiesce", "--runtime-uid", prepared.RuntimeUID, "--generation", "7", "--cleanup"}
	original := script.handler
	script.handler = func(command recordedPodCommand) ([]byte, error) {
		if !reflect.DeepEqual(command.argv, expectedArgv) {
			return original(command)
		}
		var request fuseprotocol.ProbeControlRequest
		require.NoError(t, fuseprotocol.DecodeExact(command.stdin, &request))
		require.True(t, fuseprotocol.VerifyProbeControl(rt.workspaceControlKey.Public().(ed25519.PublicKey), request, "cleanup", prepared.RuntimeUID, 7))
		require.Equal(t, expectedArgv, command.argv)
		return []byte(`{"version":1,"runtime_uid":"pod-uid-a","generation":7,"ok":true,"token":"cleanup.proof"}`), nil
	}
	_, err = rt.QuiesceWorkspaceForCleanup(context.Background(), sandboxruntime.RuntimeRef{ID: prepared.RuntimeID, UID: prepared.RuntimeUID}, 7)
	require.NoError(t, err)
	rt.workspaceControlKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x7b}, ed25519.SeedSize))
	require.NotEqual(t, rt.WarmPoolContract(), (&Runtime{namespace: rt.namespace}).WarmPoolContract())
}

func TestWorkspaceControlAuthorityWithRedisAcrossClients(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	ctx := context.Background()
	firstStore, err := redisstate.New(ctx, redisstate.Options{Addr: addr})
	require.NoError(t, err)
	defer func() { require.NoError(t, firstStore.Close()) }()
	scope := "control-authority-test/" + uuid.NewString()
	digest := sha256.Sum256([]byte(scope))
	keyName := "sandbox:control-authority:v1:{" + hex.EncodeToString(digest[:]) + "}"
	defer func() { require.NoError(t, firstStore.Delete(ctx, keyName)) }()
	key, err := LoadWorkspaceControlKey(ctx, firstStore, scope)
	require.NoError(t, err)
	public := key.Public().(ed25519.PublicKey)
	clear(key)
	secondStore, err := redisstate.New(ctx, redisstate.Options{Addr: addr})
	require.NoError(t, err)
	defer func() { require.NoError(t, secondStore.Close()) }()
	restored, err := LoadWorkspaceControlKey(ctx, secondStore, scope)
	require.NoError(t, err)
	defer clear(restored)
	request, err := fuseprotocol.SignProbeControl(restored, "cleanup", "uid-a", 7)
	require.NoError(t, err)
	require.True(t, fuseprotocol.VerifyProbeControl(public, request, "cleanup", "uid-a", 7), "a new API connection must preserve the original runtime authority")
}
