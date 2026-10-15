package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

func cleanupBinding() TaskRuntimeCleanupBinding {
	return TaskRuntimeCleanupBinding{Authority: p.TrustBinding{Namespace: "/sandbox/scope/cell/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Runtime: p.RuntimeReference{ID: "runtime-a", UID: "22222222-2222-4222-8222-222222222222", BootID: "33333333-3333-4333-8333-333333333333"}, Generation: 1, DataGateEpoch: 1, TaskID: "44444444-4444-4444-8444-444444444444", LogicalID: "logical-a", WorkspaceHash: strings.Repeat("a", 64), CreationIntentID: "55555555-5555-4555-8555-555555555555", NodeUID: "66666666-6666-4666-8666-666666666666", NodeBootID: "77777777-7777-4777-8777-777777777777", EnrollmentDigest: strings.Repeat("b", 64), ContractDigest: strings.Repeat("c", 64)}
}

func TestTaskRuntimeCleanupClientNamespace(t *testing.T) {
	s, count := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(s.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	long := "/" + strings.Repeat("p", 499) + "/scope/cell/"
	require.Len(t, long, 512)
	for _, namespace := range []string{cleanupBinding().Authority.Namespace, long} {
		t.Run(namespace[:8], func(t *testing.T) {
			binding := cleanupBinding()
			binding.Authority.Namespace = namespace
			h, err := client.Prepare(context.Background(), binding, nil)
			require.NoError(t, err)
			var descriptor taskCleanupDescriptor
			require.NoError(t, json.Unmarshal(h.Descriptor(), &descriptor))
			require.Equal(t, "runtimes", descriptor.Namespace)
			require.Equal(t, namespace, descriptor.Binding.Authority.Namespace)
			binding.Authority.Namespace = "/sandbox/scope/other/"
			before := count.Load()
			_, err = client.Prepare(context.Background(), binding, h.Descriptor())
			require.Error(t, err, "canonical descriptor retains exact authority namespace")
			require.Equal(t, before, count.Load())
		})
	}
	for _, namespace := range []string{"/" + strings.Repeat("p", 500) + "/scope/cell/", "/sandbox/scope/cell", "sandbox/scope/cell/", "/scope/cell/", "/sandbox//cell/", "/sandbox/../cell/", "/sandbox/scope/" + strings.Repeat("c", 129) + "/"} {
		binding := cleanupBinding()
		binding.Authority.Namespace = namespace
		before := count.Load()
		_, err := client.Prepare(context.Background(), binding, nil)
		require.Error(t, err)
		require.Equal(t, before, count.Load())
	}
	binding := cleanupBinding()
	binding.TaskID = "task-alias"
	_, err = client.Prepare(context.Background(), binding, nil)
	require.Error(t, err, "TaskID is a canonical nonzero UUID")
}

func TestTaskRuntimeCleanupClientAuthorityIdentity(t *testing.T) {
	s, count := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(s.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	binding := cleanupBinding()
	binding.Authority.AuthorityID = "authority/控制"
	binding.Authority.Target = "target /opaque"
	h, err := client.Prepare(context.Background(), binding, nil)
	require.NoError(t, err, "opaque authority IDs and non-UUID restore epoch are genuine bindings")
	var descriptor taskCleanupDescriptor
	require.NoError(t, json.Unmarshal(h.Descriptor(), &descriptor))
	require.Equal(t, binding, descriptor.Binding)
	for _, invalid := range []string{"", ".", "..", "restore/path", strings.Repeat("x", 129), "restore\n"} {
		bad := binding
		bad.Authority.RestoreEpoch = invalid
		before := count.Load()
		_, err := client.Prepare(context.Background(), bad, nil)
		require.Error(t, err)
		require.Equal(t, before, count.Load())
	}
	for _, invalid := range []string{"", "bad\n", "bad\x00", string([]byte{0xff}), strings.Repeat("x", 129)} {
		for _, target := range []bool{false, true} {
			bad := binding
			if target {
				bad.Authority.Target = invalid
			} else {
				bad.Authority.AuthorityID = invalid
			}
			before := count.Load()
			_, err := client.Prepare(context.Background(), bad, nil)
			require.Error(t, err)
			require.Equal(t, before, count.Load())
		}
	}
}

func cleanupPod() *v1.Pod {
	b := cleanupBinding()
	no, yes, zero := false, true, int64(0)
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.Runtime.ID, Namespace: "runtimes", UID: "22222222-2222-4222-8222-222222222222", ResourceVersion: "1", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": b.LogicalID}, Annotations: map[string]string{"sandbox.creation.intent": b.CreationIntentID}}, Spec: v1.PodSpec{NodeName: "node-a", RestartPolicy: v1.RestartPolicyNever, AutomountServiceAccountToken: &no, EnableServiceLinks: &no, SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}, Containers: []v1.Container{{Name: "sandbox", Image: "image@sha256:" + strings.Repeat("a", 64), SecurityContext: &v1.SecurityContext{RunAsUser: &zero, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}}}}}}
}

func cleanupNode() *v1.Node {
	b := cleanupBinding()
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: "66666666-6666-4666-8666-666666666666", ResourceVersion: "1"}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: b.NodeBootID, OperatingSystem: "linux", Architecture: "amd64"}}}
}

func cleanupServer(t *testing.T, pod *v1.Pod) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := &atomic.Int32{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/runtimes/pods/runtime-a":
			require.NoError(t, json.NewEncoder(w).Encode(pod))
		case "/api/v1/nodes/node-a":
			require.NoError(t, json.NewEncoder(w).Encode(cleanupNode()))
		default:
			w.WriteHeader(http.StatusNotFound)
			require.NoError(t, json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404}))
		}
	}))
	t.Cleanup(s.Close)
	return s, count
}

func cleanupOptions(host string) TaskRuntimeCleanupOptions {
	return TaskRuntimeCleanupOptions{Config: &rest.Config{Host: host}, Namespace: "runtimes", MaxActive: 2, RequestTimeout: time.Second}
}

func TestTaskRuntimeCleanupClientProfile(t *testing.T) {
	s, count := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(s.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.Zero(t, count.Load(), "constructor has no API effects")
	h, err := client.Prepare(context.Background(), cleanupBinding(), nil)
	require.NoError(t, err)
	obs, err := h.Observe(context.Background())
	require.NoError(t, err)
	require.False(t, obs.Terminal)
	require.Equal(t, cleanupBinding().Runtime.UID, obs.UID)
	for _, mutate := range []func(*v1.Pod){
		func(pod *v1.Pod) { pod.UID = "replacement" },
		func(pod *v1.Pod) { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
		func(pod *v1.Pod) { pod.Spec.HostPID = true },
		func(pod *v1.Pod) {
			pod.Spec.Volumes = []v1.Volume{{Name: "foreign", VolumeSource: v1.VolumeSource{CSI: &v1.CSIVolumeSource{Driver: "foreign"}}}}
		},
		func(pod *v1.Pod) { pod.Finalizers = []string{"foreign/finalizer"} },
		func(pod *v1.Pod) { pod.Annotations["sandbox.creation.intent"] = "other" },
	} {
		pod := cleanupPod()
		mutate(pod)
		badServer, _ := cleanupServer(t, pod)
		badClient, err := NewTaskRuntimeCleanupClient(cleanupOptions(badServer.URL))
		require.NoError(t, err)
		_, err = badClient.Prepare(context.Background(), cleanupBinding(), nil)
		require.Error(t, err)
		require.NoError(t, badClient.Close())
	}
	for _, mutate := range []func(*TaskRuntimeCleanupOptions){
		func(o *TaskRuntimeCleanupOptions) { o.Config = nil },
		func(o *TaskRuntimeCleanupOptions) { o.MaxActive = 0 },
		func(o *TaskRuntimeCleanupOptions) { o.MaxActive = 33 },
		func(o *TaskRuntimeCleanupOptions) { o.RequestTimeout = 6 * time.Second },
		func(o *TaskRuntimeCleanupOptions) { o.Config.BearerToken = strings.Repeat("x", 262145) },
		func(o *TaskRuntimeCleanupOptions) {
			o.Config.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return rt }
		},
		func(o *TaskRuntimeCleanupOptions) { o.Config.TLSClientConfig.CAFile = "/not-read" },
	} {
		o := cleanupOptions(s.URL)
		mutate(&o)
		_, err := NewTaskRuntimeCleanupClient(o)
		require.Error(t, err)
	}
}

func TestTaskRuntimeCleanupClientDescriptor(t *testing.T) {
	s, count := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(s.URL))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	h, err := client.Prepare(context.Background(), cleanupBinding(), nil)
	require.NoError(t, err)
	wire := h.Descriptor()
	require.NotEmpty(t, wire)
	copyWire := h.Descriptor()
	copyWire[0] = '!'
	require.Equal(t, wire, h.Descriptor())
	_, err = client.Prepare(context.Background(), cleanupBinding(), wire)
	require.NoError(t, err)
	for _, invalid := range [][]byte{[]byte("null"), []byte("{}"), []byte(strings.Repeat("x", 16385)), append([]byte(" "), wire...), append(wire[:len(wire)-1:len(wire)-1], []byte(",\"unknown\":1}")...)} {
		before := count.Load()
		_, err = client.Prepare(context.Background(), cleanupBinding(), invalid)
		require.Error(t, err)
		require.Equal(t, before, count.Load(), "invalid saved descriptor fails before IO")
	}
	clone := new(PreparedTaskRuntimeCleanup)
	reflect.ValueOf(clone).Elem().Set(reflect.ValueOf(h).Elem())
	_, err = clone.Observe(context.Background())
	require.Error(t, err)
	var zero PreparedTaskRuntimeCleanup
	_, err = zero.Observe(context.Background())
	require.Error(t, err)
}

func TestTaskRuntimeCleanupClientOwners(t *testing.T) {
	s, _ := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(s.URL))
	require.NoError(t, err)
	copyClient := new(TaskRuntimeCleanupClient)
	reflect.ValueOf(copyClient).Elem().Set(reflect.ValueOf(client).Elem())
	_, err = copyClient.Prepare(context.Background(), cleanupBinding(), nil)
	require.Error(t, err)
	require.Error(t, copyClient.Close())
	var zero TaskRuntimeCleanupClient
	require.Error(t, zero.Close())
	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
	_, err = client.Prepare(context.Background(), cleanupBinding(), nil)
	require.Error(t, err)
}

type cleanupRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cleanupRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cleanupBlockedBody struct {
	entered chan struct{}
	release chan struct{}
	reader  io.Reader
}

func (b *cleanupBlockedBody) Read(p []byte) (int, error) {
	select {
	case <-b.entered:
	default:
		close(b.entered)
	}
	<-b.release
	return b.reader.Read(p)
}

func (*cleanupBlockedBody) Close() error { return nil }

func TestTaskRuntimeCleanupClientOwnersJoin(t *testing.T) {
	options := cleanupOptions("http://127.0.0.1:1")
	options.MaxActive = 1
	client, err := NewTaskRuntimeCleanupClient(options)
	require.NoError(t, err)
	wire, err := json.Marshal(cleanupPod())
	require.NoError(t, err)
	body := &cleanupBlockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(string(wire))}
	// This private fault seam blocks the actual SDK body read. The public
	// constructor rejects injected transports and never accepts this substitute.
	client.api.Client.Transport = cleanupRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body}, nil
	})
	operation := make(chan error, 1)
	go func() { _, err := client.Prepare(context.Background(), cleanupBinding(), nil); operation <- err }()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("original body did not start")
	}
	_, err = client.Prepare(context.Background(), cleanupBinding(), nil)
	require.Error(t, err, "actual owner consumes the sole slot")
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before original body joined: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_, err = client.Prepare(context.Background(), cleanupBinding(), nil)
	require.Error(t, err, "late owner must be sealed")
	close(body.release)
	require.Error(t, <-operation)
	require.NoError(t, <-closed)
}
