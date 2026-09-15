package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/redisbootstrap"
)

var errRedisBootstrapGate = errors.New("redis initialization gate is unconfirmed")

const redisBootstrapGateTimeout = 10 * time.Minute

type redisBootstrapGateDependencies struct {
	waitInitialized func(context.Context, redisbootstrap.BootstrapFilePaths) error
	readFiles       func(context.Context, redisbootstrap.BootstrapFilePaths) (redisbootstrap.BootstrapFiles, error)
	verifyTopology  func(context.Context, redisbootstrap.TopologyOptions, bool) error
	waitRetry       func(context.Context) error
}

var defaultRedisBootstrapGateDependencies = redisBootstrapGateDependencies{
	waitInitialized: redisbootstrap.WaitBootstrapInitialized,
	readFiles:       redisbootstrap.ReadBootstrapFiles,
	verifyTopology:  redisbootstrap.VerifyTopology,
	waitRetry: func(ctx context.Context) error {
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	},
}

// Cleanup uses the existing Redis drain safety contract, not the business
// initialization gate: unfinished initialization must remain drainable.
func waitForRedisBootstrap(ctx context.Context, c config.RedisConfig, cleanup bool) error {
	return waitForRedisBootstrapWithDependencies(ctx, c, cleanup, defaultRedisBootstrapGateDependencies)
}

func waitForRedisBootstrapWithDependencies(ctx context.Context, c config.RedisConfig, cleanup bool, dependencies redisBootstrapGateDependencies) error {
	if cleanup {
		return nil
	}
	if c.BootstrapStateDirectory == "" && c.BootstrapPublicKeysFile == "" {
		return nil
	}
	if ctx == nil || dependencies.waitInitialized == nil || dependencies.readFiles == nil || dependencies.verifyTopology == nil || dependencies.waitRetry == nil || c.Mode != "sentinel" || !c.RequireHA || c.Durability != "replica_ack" || c.AckReplicas != 1 || len(c.Addrs) != 3 {
		return errRedisBootstrapGate
	}
	if !filepath.IsAbs(c.BootstrapStateDirectory) || filepath.Clean(c.BootstrapStateDirectory) == string(filepath.Separator) {
		return errRedisBootstrapGate
	}
	ctx, cancel := context.WithTimeout(ctx, redisBootstrapGateTimeout)
	defer cancel()
	paths := redisbootstrap.BootstrapFilePaths{Cluster: filepath.Join(c.BootstrapStateDirectory, "cluster.json"), Registration: filepath.Join(c.BootstrapStateDirectory, "registration.json"), PublicKeys: c.BootstrapPublicKeysFile}
	if err := dependencies.waitInitialized(ctx, paths); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRedisBootstrapGate
	}
	files, err := dependencies.readFiles(ctx, paths)
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
	options := redisbootstrap.TopologyOptions{
		Registration:     *files.Registration,
		PublicKeys:       files.PublicKeys,
		MasterName:       c.MasterName,
		DataPassword:     c.Password,
		SentinelPassword: c.SentinelPassword,
		AckTimeout:       time.Duration(c.AckTimeoutMS) * time.Millisecond,
	}
	for {
		if err := dependencies.verifyTopology(ctx, options, true); err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := dependencies.waitRetry(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errRedisBootstrapGate
		}
	}
	current, err := dependencies.readFiles(ctx, paths)
	if err != nil || !sameRedisBootstrapFiles(files, current) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRedisBootstrapGate
	}
	return nil
}

func sameRedisBootstrapFiles(a, b redisbootstrap.BootstrapFiles) bool {
	if a.Cluster != b.Cluster || a.Registration == nil || b.Registration == nil || *a.Registration != *b.Registration {
		return false
	}
	for i := range a.PublicKeys {
		if !bytes.Equal(a.PublicKeys[i], b.PublicKeys[i]) {
			return false
		}
	}
	return true
}
