package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

func initializeCommandFixture(t *testing.T) ([]string, *fake.Clientset) {
	t.Helper()
	t.Setenv("REDIS_PASSWORD", strings.Repeat("d", 32))
	t.Setenv("REDIS_SENTINEL_PASSWORD", strings.Repeat("s", 32))
	t.Setenv("POD_NAMESPACE", "isolated")
	secret, err := redisbootstrap.GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public-keys.json")
	if err := os.WriteFile(path, secret.Data["public-keys.json"], 0600); err != nil {
		t.Fatal(err)
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := redisbootstrap.PublicKeySetDigest(keys)
	if err != nil {
		t.Fatal(err)
	}
	c := redisbootstrap.ClusterState{ClusterID: "command-fixture", Members: [3]string{"redis-0", "redis-1", "redis-2"}, Phase: redisbootstrap.Pending}
	r := redisbootstrap.BootstrapRegistration{Cluster: c, KeyDigest: digest, MarkerIDs: [3]string{strings.Repeat("1", 32), strings.Repeat("2", 32), strings.Repeat("3", 32)}}
	cluster, _ := json.Marshal(c)
	registration, _ := json.Marshal(r)
	cm := &api.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "isolated", Name: "identity", UID: "command-uid", ResourceVersion: "1"}, Data: map[string]string{"cluster.json": string(cluster), "registration.json": string(registration)}}
	return []string{"-state-configmap", "identity", "-public-keys-file", path, "-timeout", "1s"}, fake.NewClientset(cm)
}

func TestInitializeCommandParsesAndScopesWithoutCredentialArguments(t *testing.T) {
	args, client := initializeCommandFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := 0
	var output bytes.Buffer
	err := runInitializeWithClient(ctx, args, &output, func(_ context.Context, namespace string) (corev1.ConfigMapInterface, error) {
		called++
		if namespace != "isolated" {
			t.Fatal("factory must use explicit/downward namespace")
		}
		return client.CoreV1().ConfigMaps(namespace), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || called != 1 || output.Len() != 0 {
		t.Fatalf("valid command must invoke scoped initializer and preserve caller wait, calls=%d err=%v", called, err)
	}
	if len(client.Actions()) == 0 {
		t.Fatal("bounded real initializer must read existing scoped ConfigMap")
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" || a.GetNamespace() != "isolated" || a.GetResource().Resource != "configmaps" {
			t.Fatal("unconfirmed command attempted non-read/scoped resource")
		}
	}
}

func TestInitializeCommandStaticHelpDoesNotCreateClient(t *testing.T) {
	var out bytes.Buffer
	called := false
	err := runInitializeWithClient(context.Background(), []string{"-help"}, &out, func(context.Context, string) (corev1.ConfigMapInterface, error) {
		called = true
		return nil, errors.New("PRIVATE-FACTORY-DETAIL")
	})
	if err != nil || called || !strings.Contains(out.String(), "REDIS_SENTINEL_PASSWORD") {
		t.Fatal("standalone help must be static with env-only password instructions")
	}
}

func TestInitializeCommandDispatchHelp(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"initialize", "-help"}, &out); err != nil || !strings.Contains(out.String(), "state-configmap") {
		t.Fatal("initialize mode must dispatch static help without cluster reads")
	}
}

func TestInitializeCommandMalformedAndCancelledDoesNoClientIO(t *testing.T) {
	for _, name := range []string{"namespace", "name", "missing name", "unknown password flag", "trailing", "combined help", "relative keys", "password", "same passwords", "ack", "timeout", "cancelled", "nil context"} {
		t.Run(name, func(t *testing.T) {
			args, _ := initializeCommandFixture(t)
			ctx := context.Background()
			switch name {
			case "namespace":
				args = append(args, "-namespace", "other.namespace")
			case "name":
				args = append(args, "-state-configmap", "PRIVATE-FACTORY-DETAIL\n")
			case "missing name":
				args = args[2:]
			case "unknown password flag":
				args = append(args, "-password", "PRIVATE-FACTORY-DETAIL")
			case "trailing":
				args = append(args, "PRIVATE-FACTORY-DETAIL")
			case "combined help":
				args = []string{"-help", "-password", "PRIVATE-FACTORY-DETAIL"}
			case "relative keys":
				args = append(args, "-public-keys-file", "relative")
			case "password":
				t.Setenv("REDIS_PASSWORD", "PRIVATE-FACTORY-DETAIL")
			case "same passwords":
				t.Setenv("REDIS_SENTINEL_PASSWORD", os.Getenv("REDIS_PASSWORD"))
			case "ack":
				args = append(args, "-ack-timeout", "1ms")
			case "timeout":
				args = append(args, "-timeout", "13m")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil context":
				ctx = nil
			}
			called := false
			var out bytes.Buffer
			err := runInitializeWithClient(ctx, args, &out, func(context.Context, string) (corev1.ConfigMapInterface, error) {
				called = true
				return nil, errors.New("PRIVATE-FACTORY-DETAIL")
			})
			if err == nil || called || out.Len() != 0 || strings.Contains(err.Error(), "PRIVATE-FACTORY-DETAIL") {
				t.Fatal("invalid command read client or echoed input")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("caller cancellation lost")
			}
		})
	}
}
