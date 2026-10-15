package kubernetes

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTaskRuntimeCleanupClientNoReplay(t *testing.T) {
	for _, failure := range []string{"429", "307", "308", "broken-connection"} {
		for _, step := range cleanupSteps {
			t.Run(failure+"/"+string(step), func(t *testing.T) {
				f, options, binding := newCleanupAPIFixture(t, false)
				options.RequestTimeout = 3 * time.Second
				if step != TaskRuntimeCleanupInstallRetainFinalizer {
					f.pod.Finalizers = []string{taskCleanupFinalizer}
				}
				if step != TaskRuntimeCleanupInstallRetainFinalizer && step != TaskRuntimeCleanupDeletePod {
					now := metav1.Now()
					f.pod.DeletionTimestamp = &now
				}
				f.mutation = func(w http.ResponseWriter, r *http.Request) bool {
					switch failure {
					case "429":
						w.Header().Set("Retry-After", "1")
						w.WriteHeader(429)
						json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonTooManyRequests, Code: 429})
					case "307", "308":
						w.Header().Set("Location", f.server.URL+"/replayed")
						if failure == "307" {
							w.WriteHeader(307)
						} else {
							w.WriteHeader(308)
						}
					case "broken-connection":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return true
						}
						conn.Close()
					}
					return true
				}
				client, err := NewTaskRuntimeCleanupClient(options)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				h, err := client.Prepare(context.Background(), binding, nil)
				require.NoError(t, err)
				snapshot, err := h.ObserveStep(context.Background(), step)
				require.NoError(t, err)
				attempt, err := h.Apply(context.Background(), step)
				require.Error(t, err)
				require.Equal(t, snapshot, attempt)
				require.Len(t, f.receipts(), 1, "counts actual TLS server receipts, not wrappers")
				_, err = h.ObserveStep(context.Background(), step)
				require.NoError(t, err)
				_, err = h.Apply(context.Background(), step)
				require.Error(t, err)
				require.Len(t, f.receipts(), 1, "unknown remains consumed after reconciliation")
				t.Logf("actual received mutation requests=%d", len(f.receipts()))
			})
		}
	}
}

func TestTaskRuntimeCleanupClientResponseCaps(t *testing.T) {
	for _, status := range []int{200, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var oversized atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/namespaces/runtimes/pods/runtime-a":
					if oversized.Load() {
						w.WriteHeader(status)
						w.(http.Flusher).Flush() // unknown/chunked length bypasses Content-Length checks
						if n, err := w.Write(bytes.Repeat([]byte("x"), taskCleanupBodyLimit+1)); err != nil {
							t.Logf("hard-cap peer closed after %d response bytes", n)
						}
					} else {
						json.NewEncoder(w).Encode(cleanupPod())
					}
				case "/api/v1/nodes/node-a":
					json.NewEncoder(w).Encode(cleanupNode())
				default:
					w.WriteHeader(404)
					json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
				}
			}))
			t.Cleanup(server.Close)
			client, err := NewTaskRuntimeCleanupClient(cleanupOptions(server.URL))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			h, err := client.Prepare(context.Background(), cleanupBinding(), nil)
			require.NoError(t, err)
			oversized.Store(true)
			observation, err := h.Observe(context.Background())
			require.Error(t, err, "a chunked oversized 404 is a hard failure, never absence")
			require.False(t, observation.Absent)
		})
	}
}

func TestTaskRuntimeCleanupClientDialJoin(t *testing.T) {
	options := cleanupOptions("http://127.0.0.1:1")
	options.MaxActive = 1
	client, err := NewTaskRuntimeCleanupClient(options)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var dialCalls atomic.Int32
	client.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		_, done, err := client.trackDial(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
		if dialCalls.Add(1) == 1 {
			close(entered)
		}
		<-release // the original local dial/signing callback ignores cancellation
		return nil, errors.New("fixture original dial completed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	operation := make(chan error, 1)
	go func() { _, err := client.Prepare(ctx, cleanupBinding(), nil); operation <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("original dial did not start")
	}
	cancel()
	early := false
	select {
	case <-operation:
		early = true
		t.Error("operation returned before its original dial joined")
	case <-time.After(30 * time.Millisecond):
	}
	client.mu.Lock()
	active := client.active
	client.mu.Unlock()
	if active != 1 {
		t.Errorf("cancelled parent refunded original owner slot: active=%d", active)
	}
	closed := make(chan error, 1)
	go func() { closed <- client.Close() }()
	select {
	case err := <-closed:
		t.Errorf("Close returned before original dial: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if !early {
		require.Error(t, <-operation)
	}
	require.NoError(t, <-closed)
	require.Equal(t, int32(1), dialCalls.Load())
}

func TestTaskRuntimeCleanupClientTLSImmutable(t *testing.T) {
	f, options, binding := newCleanupAPIFixture(t, false)
	certificate := f.server.TLS.Certificates[0]
	options.Config.CertData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	require.NoError(t, err)
	options.Config.KeyData = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	options.Config.NextProtos = []string{"http/1.1"}
	ca, cert, private := bytes.Clone(options.Config.CAData), bytes.Clone(options.Config.CertData), bytes.Clone(options.Config.KeyData)
	client, err := NewTaskRuntimeCleanupClient(options)
	require.NoError(t, err)
	_, err = client.Prepare(context.Background(), binding, nil)
	require.NoError(t, err)
	require.NoError(t, client.Close())
	require.Equal(t, ca, options.Config.CAData)
	require.Equal(t, cert, options.Config.CertData)
	require.Equal(t, private, options.Config.KeyData)
	require.Equal(t, []string{"http/1.1"}, options.Config.NextProtos)
	require.Equal(t, f.server.URL, options.Config.Host)
	for _, invalid := range [][]byte{[]byte("null"), []byte(strings.Repeat("x", taskCleanupDescriptorLimit+1))} {
		_, err := client.Prepare(context.Background(), binding, invalid)
		require.Error(t, err)
	}
}
