//go:build !linux

package main

import (
	"context"
	"errors"
)

func serveCleanupWorker(context.Context, string) error {
	return errors.New("cleanup worker requires Linux")
}

func serveColdQuotaCleanupWorker(context.Context, string) error {
	return errors.New("cleanup worker requires Linux")
}
