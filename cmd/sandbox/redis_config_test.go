package main

import (
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/config"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/stretchr/testify/require"
)

func TestRedisOptionsFromConfig(t *testing.T) {
	c := config.RedisConfig{
		Mode: "sentinel", Addr: "unused:6379", Addrs: []string{"observer-a:26379", "observer-b:26379"},
		MasterName: "sandbox", Username: "data", Password: "data-secret",
		SentinelUsername: "observer", SentinelPassword: "observer-secret", DB: 3,
		Durability: "replica_ack", AckReplicas: 1, AckTimeoutMS: 123,
		PoolSize: 12, MinIdleConns: 4, DialTimeoutMS: 201, ReadTimeoutMS: 202,
		WriteTimeoutMS: 203, MaxRetries: 5,
	}
	got := redisOptionsFromConfig(c)
	require.Equal(t, redisstate.Options{
		Mode: redisstate.ModeSentinel, Addr: c.Addr, Addrs: c.Addrs, MasterName: "sandbox",
		Username: "data", Password: "data-secret", SentinelUsername: "observer", SentinelPassword: "observer-secret",
		DB: 3, Durability: redisstate.DurabilityReplicaAck, AckReplicas: 1, AckTimeout: 123 * time.Millisecond,
		PoolSize: 12, MinIdleConns: 4, DialTimeout: 201 * time.Millisecond, ReadTimeout: 202 * time.Millisecond,
		WriteTimeout: 203 * time.Millisecond, MaxRetries: 5,
	}, got)
	c.Addrs[0] = "changed:26379"
	require.Equal(t, "observer-a:26379", got.Addrs[0])
}

func TestRedisOptionsFromConfigDoesNotReuseDataCredentials(t *testing.T) {
	got := redisOptionsFromConfig(config.RedisConfig{Username: "data", Password: "data-secret"})
	require.Empty(t, got.SentinelUsername)
	require.Empty(t, got.SentinelPassword)
}
