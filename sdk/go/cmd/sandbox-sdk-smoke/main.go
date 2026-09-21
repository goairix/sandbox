// Command sandbox-sdk-smoke performs a small, destructive-only-to-itself
// acceptance test against a running Sandbox API deployment.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

const (
	apiURLEnv    = "SANDBOX_API_URL"
	apiKeyEnv    = "SANDBOX_API_KEY"
	prefixEnv    = "SANDBOX_TEST_PREFIX"
	defaultLimit = 5 * time.Minute
)

func main() {
	apiURL := strings.TrimSpace(os.Getenv(apiURLEnv))
	apiKey := strings.TrimSpace(os.Getenv(apiKeyEnv))
	if apiURL == "" || apiKey == "" {
		log.Fatalf("%s and %s must be set", apiURLEnv, apiKeyEnv)
	}

	prefix := strings.TrimSpace(os.Getenv(prefixEnv))
	if prefix == "" {
		prefix = "sdk-smoke-" + time.Now().UTC().Format("20060102T150405Z")
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultLimit)
	defer cancel()
	client := sandbox.NewClient(apiURL, apiKey, sandbox.WithTimeout(45*time.Second))
	if err := runSmoke(ctx, client, prefix); err != nil {
		log.Fatal(err)
	}
}

func runSmoke(ctx context.Context, client *sandbox.Client, prefix string) error {
	if err := runCase(ctx, client, "ordinary", sandbox.SandboxOptions{
		Mode:               sandbox.ModeEphemeral,
		Timeout:            180,
		WorkspacePath:      prefix + "/sync",
		WorkspaceMountMode: sandbox.WorkspaceMountSync,
	}, "ordinary-pass", false); err != nil {
		return err
	}

	return runCase(ctx, client, "fuse", sandbox.SandboxOptions{
		Mode:               sandbox.ModePersistent,
		Timeout:            300,
		WorkspacePath:      prefix + "/fuse",
		WorkspaceMountMode: sandbox.WorkspaceMountFUSE,
	}, "fuse-pass", true)
}

func runCase(ctx context.Context, client *sandbox.Client, label string, opts sandbox.SandboxOptions, expected string, requireFUSE bool) (err error) {
	sb, err := client.NewSandbox(ctx, opts)
	if err != nil {
		return fmt.Errorf("%s: create sandbox: %w", label, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cleanupErr := sb.Close(cleanupCtx); cleanupErr != nil {
			cleanupErr = fmt.Errorf("%s: destroy sandbox: %w", label, cleanupErr)
			err = errors.Join(err, cleanupErr)
		}
	}()

	code := fmt.Sprintf("printf '%s'", expected)
	if requireFUSE {
		code = fmt.Sprintf("printf '%s' > /workspace/sdk-e2e.txt && cat /workspace/sdk-e2e.txt", expected)
	}
	result, err := sb.Run(ctx, "bash", code)
	if err != nil {
		return fmt.Errorf("%s: execute: %w", label, err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != expected {
		return fmt.Errorf("%s: unexpected execution result: exit_code=%d stdout=%q stderr=%q", label, result.ExitCode, result.Stdout, result.Stderr)
	}

	if requireFUSE {
		if _, err := sb.Sync(ctx); err != nil {
			return fmt.Errorf("%s: sync workspace: %w", label, err)
		}
		info, err := sb.WorkspaceInfo(ctx)
		if err != nil {
			return fmt.Errorf("%s: workspace info: %w", label, err)
		}
		if err := validateFUSEWorkspace(info); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}

	fmt.Printf("SDK smoke test: %s PASS (sandbox=%s)\n", label, sb.ID())
	return nil
}

func validateFUSEWorkspace(info sandbox.WorkspaceInfoResponse) error {
	if !info.Mounted || info.MountType != "fuse" || info.MountState != "ready" || !info.Flushed {
		return fmt.Errorf("FUSE workspace not ready: mounted=%t mount_type=%q mount_state=%q flushed=%t", info.Mounted, info.MountType, info.MountState, info.Flushed)
	}
	return nil
}
