package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func TestEnsureIdentityCLIValidatesBeforeClient(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-secret", "PRIVATE-MISPLACED-CREDENTIAL"}, {"-timeout", "3m"}, {"-members-json", "[\"redis-0\",\"redis-1\"]"}} {
		t.Run(args[0], func(t *testing.T) {
			var out bytes.Buffer
			called := false
			err := runEnsureIdentityWithClient(context.Background(), args, &out, func(context.Context) (corev1.CoreV1Interface, error) {
				called = true
				return nil, errors.New("PRIVATE-API")
			})
			if called {
				t.Fatal("credentials/API accessed before arguments validated")
			}
			if (err == nil) != (args[0] == "--help") {
				t.Fatal("wrong argument outcome", err)
			}
			if bytes.Contains(out.Bytes(), []byte("PRIVATE")) {
				t.Fatal("input leaked")
			}
		})
	}
}

func TestEnsureIdentityCLIExistingSuccessSilent(t *testing.T) {
	s, err := redisbootstrap.GenerateIdentitySecret("isolated", "redis")
	if err != nil {
		t.Fatal(err)
	}
	s.UID = "original"
	s.ResourceVersion = "1"
	cluster, _ := json.Marshal(redisbootstrap.ClusterState{ClusterID: "retained", Members: [3]string{"redis-0", "redis-1", "redis-2"}, Phase: redisbootstrap.Pending})
	cm := &api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "isolated", Name: "state", UID: "state-uid", ResourceVersion: "1"}, Data: map[string]string{"cluster.json": string(cluster)}}
	c := fake.NewClientset(s, cm)
	var out bytes.Buffer
	args := []string{"-namespace", "isolated", "-statefulset", "redis", "-secret", "redis-identity", "-state-configmap", "state", "-members-json", "[\"redis-0\",\"redis-1\",\"redis-2\"]"}
	if err := runEnsureIdentityWithClient(context.Background(), args, &out, func(context.Context) (corev1.CoreV1Interface, error) { return c.CoreV1(), nil }); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("identity success must not output keys")
	}
	for _, a := range c.Actions() {
		if a.GetVerb() != "get" {
			t.Fatal("existing CLI mutated resources")
		}
	}
	out.Reset()
	if err := run(context.Background(), []string{"ensure-identity", "--help"}, &out); err != nil || out.Len() == 0 {
		t.Fatal("mode not routed", err)
	}
}

func TestEnsureIdentityCLISafeAPIError(t *testing.T) {
	args := []string{"-namespace", "isolated", "-statefulset", "redis", "-secret", "redis-identity", "-state-configmap", "state", "-members-json", "[\"redis-0\",\"redis-1\",\"redis-2\"]"}
	var out bytes.Buffer
	err := runEnsureIdentityWithClient(context.Background(), args, &out, func(context.Context) (corev1.CoreV1Interface, error) { return nil, errors.New("PRIVATE-API-BODY") })
	if !errors.Is(err, redisbootstrap.ErrIdentityAPI) || out.Len() != 0 {
		t.Fatal("unsafe factory error/output", err, out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = runEnsureIdentityWithClient(ctx, args, &out, func(context.Context) (corev1.CoreV1Interface, error) { t.Fatal("access after cancel"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
