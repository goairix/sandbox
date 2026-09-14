package main

import (
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
	c := config.RedisConfig{Mode: "sentinel", MasterName: "sandbox", Password: strings.Repeat("d", 40), SentinelPassword: strings.Repeat("s", 40), RequireHA: true, Durability: "replica_ack", AckReplicas: 1, BootstrapStateDirectory: directory, BootstrapPublicKeysFile: filepath.Join(directory, "public-keys.json")}
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
	if err := waitForRedisBootstrap(context.Background(), c, false); err != nil {
		t.Fatal("valid initialized gate blocked", err)
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
