package main

import (
	"time"

	"github.com/goairix/sandbox/internal/config"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
)

func redisOptionsFromConfig(c config.RedisConfig) redisstate.Options {
	return redisstate.Options{
		Mode: redisstate.Mode(c.Mode), Addr: c.Addr, Addrs: append([]string(nil), c.Addrs...), MasterName: c.MasterName,
		Username: c.Username, Password: c.Password,
		SentinelUsername: c.SentinelUsername, SentinelPassword: c.SentinelPassword,
		DB: c.DB, Durability: redisstate.DurabilityMode(c.Durability),
		AckReplicas: c.AckReplicas, AckTimeout: time.Duration(c.AckTimeoutMS) * time.Millisecond,
		PoolSize: c.PoolSize, MinIdleConns: c.MinIdleConns,
		DialTimeout:  time.Duration(c.DialTimeoutMS) * time.Millisecond,
		ReadTimeout:  time.Duration(c.ReadTimeoutMS) * time.Millisecond,
		WriteTimeout: time.Duration(c.WriteTimeoutMS) * time.Millisecond,
		MaxRetries:   c.MaxRetries,
	}
}
