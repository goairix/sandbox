package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/redisbootstrap"
	corev1 "k8s.io/api/core/v1"
)

func TestIdentitySecretCommandGeneratesIndependentKeys(t *testing.T) {
	var output bytes.Buffer
	if err := runIdentitySecret(context.Background(), []string{"-namespace", "isolated", "-statefulset", "redis-sentinel"}, &output); err != nil {
		t.Fatalf("identity object command unavailable: %v", err)
	}
	var secret corev1.Secret
	if err := json.Unmarshal(output.Bytes(), &secret); err != nil {
		t.Fatal(err)
	}
	if secret.APIVersion != "v1" || secret.Kind != "Secret" || secret.Namespace != "isolated" || secret.Name != "redis-sentinel-identity" || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 4 || secret.Type != corev1.SecretTypeOpaque {
		t.Fatal("wrong immutable fixed Secret")
	}
	keys, err := redisbootstrap.ParseMemberPublicKeys(secret.Data["public-keys.json"])
	if err != nil {
		t.Fatal("invalid generated public keys")
	}
	for i, name := range []string{"redis-sentinel-0", "redis-sentinel-1", "redis-sentinel-2"} {
		if _, err := redisbootstrap.ParseMemberPrivateSeed(secret.Data[name], keys, i); err != nil {
			t.Fatal("seed/key ordinal binding failed")
		}
	}
}

func TestIdentitySecretCommandDispatch(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"identity-secret", "-namespace", "isolated", "-statefulset", "redis-sentinel"}, &output); err != nil {
		t.Fatalf("main dispatch unavailable: %v", err)
	}
	if !json.Valid(output.Bytes()) {
		t.Fatal("command did not output JSON")
	}
}

func TestIdentitySecretCommandRejectsArgumentsWithoutSecretOutput(t *testing.T) {
	for _, args := range [][]string{{}, {"-namespace", "isolated"}, {"-namespace", "bad-secret.namespace", "-statefulset", "redis-sentinel"}, {"-namespace", "isolated", "-statefulset", "redis-sentinel", "trailing-secret"}, {"-namespace", "isolated", "-statefulset", "redis-sentinel", "-password", "misplaced-secret"}} {
		var output bytes.Buffer
		err := runIdentitySecret(context.Background(), args, &output)
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), "misplaced-secret") || strings.Contains(err.Error(), "bad-secret") {
			t.Fatalf("bad arguments generated/leaked key material: %v", err)
		}
	}
	for _, args := range [][]string{{"-help", "-password", "misplaced-secret"}, {"-help", "trailing-secret"}} {
		var output bytes.Buffer
		if err := runIdentitySecret(context.Background(), args, &output); err == nil || output.Len() != 0 {
			t.Fatal("help hid invalid trailing arguments", err)
		}
	}
}

func TestIdentitySecretCommandHelpAndCancellation(t *testing.T) {
	var output bytes.Buffer
	if err := runIdentitySecret(context.Background(), []string{"-help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "identity-secret") || strings.Contains(output.String(), "public-keys.json") {
		t.Fatal("help generated material")
	}
	output.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runIdentitySecret(ctx, []string{"-namespace", "isolated", "-statefulset", "redis-sentinel"}, &output); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatal("canceled command generated secret", err)
	}
	if err := runIdentitySecret(context.Background(), []string{"-namespace", "isolated", "-statefulset", "redis-sentinel"}, identitySecretFailWriter{}); err == nil {
		t.Fatal("output failure swallowed")
	}
}

type identitySecretFailWriter struct{}

func (identitySecretFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
