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

func cleanupWorkerManifestArgument(args []string) (string, error) {
	if len(args) != 2 || args[0] != "serve" {
		return "", errCleanupWorkerCommand
	}
	path := args[1]
	// Match the mounted loader's 4096-byte path bound before any irreversible
	// kernel initialization. Arguments are never interpolated into diagnostics.
	if path == "" || len(path) > 4096 || !utf8.ValidString(path) || strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", errCleanupWorkerCommand
	}
	return path, nil
}

func run(ctx context.Context, args []string) error {
	path, err := cleanupWorkerManifestArgument(args)
	if err != nil {
		return err
	}
	if ctx == nil {
		return errCleanupWorkerCommand
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return serveCleanupWorker(ctx, path)
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
