package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
)

func TestPlainColdResumeDiagnostics(t *testing.T) {
	prior := PlainColdResult{
		State: PlainColdCompleted, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: "original",
		Attempt:     &state.ColdCreateAttemptRecord{Version: 1, OperationID: uuid.NewString()},
		Runtime:     p.RuntimeReference{ID: "original", UID: uuid.NewString(), BootID: uuid.NewString()},
		Publication: &state.RuntimeEnrolledPublicationEntry{},
	}
	var service *PlainColdService
	result, err := service.Resume(context.Background(), state.WorkspaceIdentity{}, prior)
	require.Error(t, err, "no service admission or Backend/native action")
	require.Equal(t, PlainColdPending, result.State)
	require.Equal(t, prior.RequestID, result.RequestID)
	require.Equal(t, prior.IntentID, result.IntentID)
	require.Equal(t, prior.Runtime, result.Runtime)
	require.Equal(t, prior.Attempt, result.Attempt)
	require.NotSame(t, prior.Attempt, result.Attempt, "diagnostic record is copied")
	require.Nil(t, result.Publication, "copied publication cannot become completion")
	result.Attempt.OperationID = "changed"
	require.NotEqual(t, result.Attempt.OperationID, prior.Attempt.OperationID)
}

func TestPlainColdServiceAdmissionClose(t *testing.T) {
	// This exercises local ownership only, with the original concrete drivers.
	// It does not construct a Backend or assert runtime creation/publication.
	api := kube.EnrolledBridgeOptions{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second}
	driver, err := kube.NewEnrolledCreateClient(kube.EnrolledCreateOptions{API: api, Image: "registry.example/sandbox:v1@sha256:" + strings.Repeat("a", 64), NodeName: "node", NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()})
	require.NoError(t, err)
	bridge, err := kube.NewEnrolledBridgeClient(api)
	require.NoError(t, err)
	s := &PlainColdService{driver: driver, bridge: bridge, root: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32)), issuer: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32)), limit: 1, active: make(map[*plainColdCall]struct{}), closeDone: make(chan struct{})}
	s.self = s
	call, err := s.enter(context.Background())
	require.NoError(t, err)
	_, err = s.enter(context.Background())
	require.Error(t, err, "bounded call admission")
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-call.context.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel its original active call")
	}
	_, err = s.enter(context.Background())
	require.Error(t, err, "Close seals fresh admission")
	select {
	case <-closed:
		t.Fatal("Close returned before the original call joined")
	default:
	}
	require.NotEqual(t, make([]byte, ed25519.PrivateKeySize), []byte(s.issuer), "keys remain owned until join")
	s.leave(call)
	require.NoError(t, <-closed)
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.root))
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.issuer))
	require.NoError(t, s.Close())
	copy := &PlainColdService{self: s}
	_, err = copy.enter(context.Background())
	require.Error(t, err, "copied service cannot acquire admission")
}
