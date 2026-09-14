package main

import (
	"context"
	"errors"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/redisbootstrap"
	"path/filepath"
	"time"
)

var errRedisBootstrapGate = errors.New("redis initialization gate is unconfirmed")

// Cleanup uses the existing Redis drain safety contract, not the business
// initialization gate: unfinished initialization must remain drainable.
func waitForRedisBootstrap(ctx context.Context, c config.RedisConfig, cleanup bool) error {
	if cleanup {
		return nil
	}
	if c.BootstrapStateDirectory == "" && c.BootstrapPublicKeysFile == "" {
		return nil
	}
	if ctx == nil || c.Mode != "sentinel" || !c.RequireHA || c.Durability != "replica_ack" || c.AckReplicas != 1 || len(c.Addrs) != 3 {
		return errRedisBootstrapGate
	}
	if !filepath.IsAbs(c.BootstrapStateDirectory) || filepath.Clean(c.BootstrapStateDirectory) == string(filepath.Separator) {
		return errRedisBootstrapGate
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	paths := redisbootstrap.BootstrapFilePaths{Cluster: filepath.Join(c.BootstrapStateDirectory, "cluster.json"), Registration: filepath.Join(c.BootstrapStateDirectory, "registration.json"), PublicKeys: c.BootstrapPublicKeysFile}
	if err := redisbootstrap.WaitBootstrapInitialized(ctx, paths); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRedisBootstrapGate
	}
	files, err := redisbootstrap.ReadBootstrapFiles(ctx, paths)
	if err != nil || files.Registration == nil || !redisbootstrap.BusinessWritersAllowed(&files.Cluster) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRedisBootstrapGate
	}
	wanted := map[string]bool{}
	for _, dns := range files.Cluster.Members {
		wanted[dns+":26379"] = true
	}
	for _, address := range c.Addrs {
		if !wanted[address] {
			return errRedisBootstrapGate
		}
		delete(wanted, address)
	}
	if len(wanted) != 0 {
		return errRedisBootstrapGate
	}
	return ctx.Err()
}
