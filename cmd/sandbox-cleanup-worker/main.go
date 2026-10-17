// sandbox-cleanup-worker runs the mounted automatic cleanup worker as an
// independent process. It never provisions metadata or accepts task references.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

var errCleanupWorkerCommand = errors.New("expected serve and one mounted manifest path")

// cleanupWorkerMode is selected by the command name, never by file contents.
// Its zero value is invalid so an unrecognized command cannot start a worker.
type cleanupWorkerMode uint8

const (
	cleanupWorkerModeAutomatic cleanupWorkerMode = iota + 1
	cleanupWorkerModeColdQuota
)

func cleanupWorkerManifestArgument(args []string) (string, error) {
	if len(args) != 2 || args[0] != "serve" {
		return "", errCleanupWorkerCommand
	}
	return cleanupWorkerManifestPath(args[1])
}

func cleanupWorkerCommand(args []string) (cleanupWorkerMode, string, error) {
	if len(args) != 2 {
		return 0, "", errCleanupWorkerCommand
	}

	var mode cleanupWorkerMode
	switch args[0] {
	case "serve":
		mode = cleanupWorkerModeAutomatic
	case "serve-cold-quota":
		mode = cleanupWorkerModeColdQuota
	default:
		return 0, "", errCleanupWorkerCommand
	}

	path, err := cleanupWorkerManifestPath(args[1])
	if err != nil {
		return 0, "", err
	}
	return mode, path, nil
}

func cleanupWorkerManifestPath(path string) (string, error) {
	// Match the mounted loader's 4096-byte path bound before any irreversible
	// kernel initialization. Arguments are never interpolated into diagnostics.
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", errCleanupWorkerCommand
	}
	return path, nil
}

func run(ctx context.Context, args []string) error {
	mode, path, err := cleanupWorkerCommand(args)
	if err != nil {
		return err
	}
	if ctx == nil {
		return errCleanupWorkerCommand
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch mode {
	case cleanupWorkerModeAutomatic:
		return serveCleanupWorker(ctx, path)
	case cleanupWorkerModeColdQuota:
		return serveColdQuotaCleanupWorker(ctx, path)
	default:
		return errCleanupWorkerCommand
	}
}

func cleanupWorkerExitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	// Mounted-file, transport and terminal errors can carry private paths or
	// upstream response text. Keep the process diagnostic fixed for all causes.
	_, _ = fmt.Fprintln(stderr, "sandbox-cleanup-worker: failed")
	return 1
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cleanupWorkerExitCode(run(ctx, os.Args[1:]), os.Stderr)
	stop()
	if code != 0 {
		os.Exit(code)
	}
}
