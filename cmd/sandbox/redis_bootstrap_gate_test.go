package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/redisbootstrap"
)

func apiBootstrapFixture(t *testing.T, phase redisbootstrap.Phase) config.RedisConfig {
	t.Helper()
	directory := t.TempDir()
	secret, err := redisbootstrap.GenerateIdentitySecret("isolated", "redis-sentinel")
	if err != nil {
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
	r := redisbootstrap.BootstrapRegistration{Cluster: redisbootstrap.ClusterState{ClusterID: "isolated", Phase: phase, Members: [3]string{"redis-0.redis-headless.isolated.svc.cluster.local", "redis-1.redis-headless.isolated.svc.cluster.local", "redis-2.redis-headless.isolated.svc.cluster.local"}}, KeyDigest: digest}
	c := config.RedisConfig{Mode: "sentinel", MasterName: "sandbox", Password: strings.Repeat("d", 40), SentinelPassword: strings.Repeat("s", 40), RequireHA: true, Durability: "replica_ack", AckReplicas: 1, AckTimeoutMS: 1000, BootstrapStateDirectory: directory, BootstrapPublicKeysFile: filepath.Join(directory, "public-keys.json")}
	for i, dns := range r.Cluster.Members {
		r.MarkerIDs[i] = fmt.Sprintf("%032x", i+1)
		c.Addrs = append(c.Addrs, dns+":26379")
	}
	cluster, _ := json.Marshal(r.Cluster)
	registration, _ := json.Marshal(r)
	for name, bytes := range map[string][]byte{"cluster.json": cluster, "registration.json": registration, "public-keys.json": secret.Data["public-keys.json"]} {
		if err := os.WriteFile(filepath.Join(directory, name), bytes, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func successfulRedisBootstrapGateDependencies() redisBootstrapGateDependencies {
	return redisBootstrapGateDependencies{
		waitInitialized: redisbootstrap.WaitBootstrapInitialized,
		readFiles:       redisbootstrap.ReadBootstrapFiles,
		verifyTopology:  func(context.Context, redisbootstrap.TopologyOptions, bool) error { return nil },
		waitRetry:       func(context.Context) error { return nil },
	}
}

func TestRedisBootstrapGateDisabledAndCleanup(t *testing.T) {
	if err := waitForRedisBootstrap(context.Background(), config.RedisConfig{}, false); err != nil {
		t.Fatal("default standalone gate changed", err)
	}
	c := apiBootstrapFixture(t, redisbootstrap.Pending)
	c.BootstrapStateDirectory = filepath.Join(c.BootstrapStateDirectory, "missing")
	if err := waitForRedisBootstrap(context.Background(), c, true); err != nil {
		t.Fatal("cleanup waited on business initialization", err)
	}
}

func TestRedisBootstrapGateInitialized(t *testing.T) {
	c := apiBootstrapFixture(t, redisbootstrap.Initialized)
	if err := waitForRedisBootstrapWithDependencies(context.Background(), c, false, successfulRedisBootstrapGateDependencies()); err != nil {
		t.Fatal("valid initialized gate blocked", err)
	}
}

func TestRedisBootstrapGateRetriesAuthenticatedTopologyWithFixedContract(t *testing.T) {
	c := apiBootstrapFixture(t, redisbootstrap.Initialized)
	paths := redisbootstrap.BootstrapFilePaths{Cluster: filepath.Join(c.BootstrapStateDirectory, "cluster.json"), Registration: filepath.Join(c.BootstrapStateDirectory, "registration.json"), PublicKeys: c.BootstrapPublicKeysFile}
	want, err := redisbootstrap.ReadBootstrapFiles(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	dependencies := successfulRedisBootstrapGateDependencies()
	attempts := 0
	dependencies.verifyTopology = func(_ context.Context, got redisbootstrap.TopologyOptions, all bool) error {
		attempts++
		if !all || got.Registration != *want.Registration || got.MasterName != c.MasterName || got.DataPassword != c.Password || got.SentinelPassword != c.SentinelPassword || got.AckTimeout != time.Second {
			t.Fatal("topology verifier did not receive the fixed HA/ACK contract")
		}
		for i := range got.PublicKeys {
			if !bytes.Equal(got.PublicKeys[i], want.PublicKeys[i]) {
				t.Fatal("topology verifier did not receive the fixed public keys")
			}
		}
		if attempts < 3 {
			return errors.New("topology temporarily unavailable")
		}
		return nil
	}
	if err := waitForRedisBootstrapWithDependencies(context.Background(), c, false, dependencies); err != nil {
		t.Fatal("valid topology did not open after retry", err)
	}
	if attempts != 3 {
		t.Fatalf("topology attempts = %d, want 3", attempts)
	}
}

func TestRedisBootstrapGateRejectsProjectionReplacementAfterTopologyProof(t *testing.T) {
	c := apiBootstrapFixture(t, redisbootstrap.Initialized)
	dependencies := successfulRedisBootstrapGateDependencies()
	reads := 0
	dependencies.readFiles = func(ctx context.Context, paths redisbootstrap.BootstrapFilePaths) (redisbootstrap.BootstrapFiles, error) {
		files, err := redisbootstrap.ReadBootstrapFiles(ctx, paths)
		if err != nil {
			return files, err
		}
		reads++
		if reads == 2 {
			files.Cluster.ClusterID = "replacement"
		}
		return files, nil
	}
	err := waitForRedisBootstrapWithDependencies(context.Background(), c, false, dependencies)
	if !errors.Is(err, errRedisBootstrapGate) {
		t.Fatal("projection replacement opened the gate", err)
	}
}

func TestRedisBootstrapGateTopologyRetryHonorsCallerDeadline(t *testing.T) {
	c := apiBootstrapFixture(t, redisbootstrap.Initialized)
	dependencies := successfulRedisBootstrapGateDependencies()
	dependencies.verifyTopology = func(context.Context, redisbootstrap.TopologyOptions, bool) error {
		return errors.New("topology unavailable")
	}
	dependencies.waitRetry = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitForRedisBootstrapWithDependencies(ctx, c, false, dependencies); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("topology retry ignored caller deadline", err)
	}
}

func TestRedisBootstrapGatePendingDoesNotOpen(t *testing.T) {
	c := apiBootstrapFixture(t, redisbootstrap.Pending)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := waitForRedisBootstrap(ctx, c, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Pending gate returned early", err)
	}
}

func TestRedisBootstrapGateBindsActualEndpoints(t *testing.T) {
	for _, mutate := range []func(*config.RedisConfig){
		func(c *config.RedisConfig) { c.Addrs[0] = "other-redis:26379" },
		func(c *config.RedisConfig) { c.Addrs[0] = c.Addrs[1] },
		func(c *config.RedisConfig) { c.Addrs = c.Addrs[:2] },
		func(c *config.RedisConfig) { c.Addrs[0] = strings.ReplaceAll(c.Addrs[0], ":26379", ":6379") },
		func(c *config.RedisConfig) { c.RequireHA = false },
		func(c *config.RedisConfig) { c.Durability = "best_effort" },
	} {
		c := apiBootstrapFixture(t, redisbootstrap.Initialized)
		mutate(&c)
		if err := waitForRedisBootstrap(context.Background(), c, false); err == nil {
			t.Fatal("gate authorized different Redis topology/contract")
		}
	}
}
