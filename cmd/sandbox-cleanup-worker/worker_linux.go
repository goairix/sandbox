//go:build linux

package main

import (
	"context"

	"github.com/goairix/sandbox/internal/provisioning/deployment"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

func serveCleanupWorker(ctx context.Context, path string) error {
	return serveCleanupWorkerMode(ctx, path, cleanupWorkerModeAutomatic)
}

func serveColdQuotaCleanupWorker(ctx context.Context, path string) error {
	return serveCleanupWorkerMode(ctx, path, cleanupWorkerModeColdQuota)
}

func serveCleanupWorkerMode(ctx context.Context, path string, mode cleanupWorkerMode) error {
	if mode != cleanupWorkerModeAutomatic && mode != cleanupWorkerModeColdQuota {
		return errCleanupWorkerCommand
	}

	// This one-shot initializer hardens and seals the actual non-root process.
	// Failure consumes initialization: returning to main terminates the role.
	// The boundary remains borrowed until the deployment has physically joined.
	kernel, err := launcher.BootstrapWorkspaceSyncWorker()
	if err != nil {
		return err
	}
	var worker *deployment.LocalCleanupDeployment
	switch mode {
	case cleanupWorkerModeAutomatic:
		worker, err = deployment.OpenAutomaticLocalCleanup(ctx, path, kernel)
	case cleanupWorkerModeColdQuota:
		worker, err = deployment.OpenColdQuotaAutomaticLocalCleanup(ctx, path, kernel)
	}
	if err != nil {
		return err
	}
	return awaitCleanupWorker(ctx, worker)
}
