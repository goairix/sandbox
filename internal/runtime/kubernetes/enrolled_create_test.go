package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func coldCreateFixture(t *testing.T, handler http.HandlerFunc) (*EnrolledCreateClient, EnrolledPlainPodRequest) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	r := enrolledPodRequest(t)
	c, err := NewEnrolledCreateClient(EnrolledCreateOptions{
		API:   EnrolledBridgeOptions{Config: &rest.Config{Host: server.URL, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second},
		Image: r.Spec.Image, NodeName: r.NodeName, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	return c, r
}

func TestColdCreateLostAcknowledgmentInspectsOriginal(t *testing.T) {
	var posts, gets atomic.Int32
	var saved corev1.Pod
	var mu sync.Mutex
	c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case http.MethodPost:
			posts.Add(1)
			if req.URL.Path != "/api/v1/namespaces/runtimes/pods" || json.NewDecoder(req.Body).Decode(&saved) != nil {
				w.WriteHeader(400)
				return
			}
			if saved.APIVersion != "v1" || saved.Kind != "Pod" {
				w.WriteHeader(400)
				return
			}
			saved.UID = types.UID("11111111-1111-4111-8111-111111111111")
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		case http.MethodGet:
			gets.Add(1)
			if req.URL.Path != "/api/v1/namespaces/runtimes/pods/runtime-a" {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(&saved)
		default:
			w.WriteHeader(405)
		}
	})
	result, err := c.CreateOnce(context.Background(), r)
	require.Error(t, err)
	require.Equal(t, EnrolledCreateUnknown, result.State)
	result, err = c.InspectOriginal(context.Background(), r, "")
	require.NoError(t, err)
	require.Equal(t, EnrolledCreateObserved, result.State)
	require.Equal(t, "11111111-1111-4111-8111-111111111111", result.PodUID)
	require.EqualValues(t, 1, posts.Load())
	require.EqualValues(t, 1, gets.Load())
	result, err = c.InspectOriginal(context.Background(), r, "22222222-2222-4222-8222-222222222222")
	require.Error(t, err)
	require.Equal(t, EnrolledCreateUnknown, result.State)
	require.Empty(t, result.PodUID)
	require.EqualValues(t, 1, posts.Load())
}

func TestColdCreateMissingRemainsUnknown(t *testing.T) {
	var posts atomic.Int32
	c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusNotFound)
	})
	result, err := c.InspectOriginal(context.Background(), r, "11111111-1111-4111-8111-111111111111")
	require.Error(t, err)
	require.Equal(t, EnrolledCreateUnknown, result.State)
	require.Empty(t, result.PodUID)
	require.Zero(t, posts.Load())
}

func TestColdCreateCloseAbortsAndJoins(t *testing.T) {
	entered, disconnected := make(chan struct{}), make(chan struct{})
	c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		close(entered)
		<-req.Context().Done()
		close(disconnected)
	})
	done := make(chan EnrolledCreateResult, 1)
	go func() { result, _ := c.CreateOnce(context.Background(), r); done <- result }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("POST not received")
	}
	second, err := c.CreateOnce(context.Background(), r)
	require.Error(t, err)
	require.Equal(t, EnrolledCreateNotSent, second.State)
	require.NoError(t, c.Close())
	select {
	case result := <-done:
		require.Equal(t, EnrolledCreateUnknown, result.State)
	case <-time.After(time.Second):
		t.Fatal("Close did not join POST")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("server did not observe physical close")
	}
	result, err := c.CreateOnce(context.Background(), r)
	require.Error(t, err)
	require.Equal(t, EnrolledCreateNotSent, result.State)
}

func TestColdCreateValidatesCompletePodAndOriginalUID(t *testing.T) {
	for _, variant := range []string{"valid", "quantity-equivalent", "changed-image", "changed-config", "changed-resources", "changed-node", "removed-finalizer"} {
		t.Run(variant, func(t *testing.T) {
			var posts atomic.Int32
			c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) {
				posts.Add(1)
				var pod corev1.Pod
				if json.NewDecoder(req.Body).Decode(&pod) != nil {
					w.WriteHeader(400)
					return
				}
				pod.UID = types.UID("11111111-1111-4111-8111-111111111111")
				switch variant {
				case "quantity-equivalent":
					pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("1")
				case "changed-image":
					pod.Spec.Containers[0].Image = "other.test/image:latest"
				case "changed-config":
					pod.Spec.Containers[0].Env[0].Value += " "
				case "changed-resources":
					pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("1Gi")
				case "changed-node":
					pod.Spec.NodeName = "node-b"
				case "removed-finalizer":
					pod.Finalizers = nil
				}
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(&pod)
			})
			r.Spec.CPU = "1000m"
			result, err := c.CreateOnce(context.Background(), r)
			if variant == "valid" || variant == "quantity-equivalent" {
				require.NoError(t, err)
				require.Equal(t, EnrolledCreateObserved, result.State)
			} else {
				require.Error(t, err)
				require.Equal(t, EnrolledCreateUnknown, result.State)
			}
			require.EqualValues(t, 1, posts.Load())
		})
	}
}

func TestColdCreateCancellationJoinsPhysicalRequest(t *testing.T) {
	entered, disconnected := make(chan struct{}), make(chan struct{})
	c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) {
		io.Copy(io.Discard, req.Body)
		close(entered)
		<-req.Context().Done()
		close(disconnected)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan EnrolledCreateResult, 1)
	go func() { result, _ := c.CreateOnce(ctx, r); done <- result }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("POST not received")
	}
	cancel()
	select {
	case result := <-done:
		require.Equal(t, EnrolledCreateUnknown, result.State)
	case <-time.After(time.Second):
		t.Fatal("cancel did not join POST")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("server did not observe physical close")
	}
}

func TestColdCreateBoundConfigurationAndCancellation(t *testing.T) {
	var calls atomic.Int32
	c, r := coldCreateFixture(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1); w.WriteHeader(500) })
	for _, mutate := range []func(*EnrolledPlainPodRequest){
		func(r *EnrolledPlainPodRequest) { r.Spec.Image = "other.test/image:latest" },
		func(r *EnrolledPlainPodRequest) { r.NodeName = "other-node" },
		func(r *EnrolledPlainPodRequest) { r.NodeUID = "55555555-5555-4555-8555-555555555555" },
		func(r *EnrolledPlainPodRequest) { r.NodeBootID = "55555555-5555-4555-8555-555555555555" },
	} {
		changed := r
		mutate(&changed)
		result, err := c.CreateOnce(context.Background(), changed)
		require.Error(t, err)
		require.Equal(t, EnrolledCreateNotSent, result.State)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := c.CreateOnce(ctx, r)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, EnrolledCreateNotSent, result.State)
	require.Zero(t, calls.Load())
}
