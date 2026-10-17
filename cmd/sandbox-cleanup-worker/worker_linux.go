//go:build linux

package main

import (
	"context"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

func serveCleanupWorker(ctx context.Context, path string) error {
	// This one-shot initializer hardens and seals the actual non-root process.
	// Failure consumes initialization: returning to main terminates the role.
	// The boundary remains borrowed until the deployment has physically joined.
	kernel, err := launcher.BootstrapWorkspaceSyncWorker()
	if err != nil {
		return err
	}
	worker, err := deployment.OpenAutomaticLocalCleanup(ctx, path, kernel)
	if err != nil {
		return err
	}
	return awaitCleanupWorker(ctx, worker)
}
