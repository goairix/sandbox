package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCleanupWorkerCommandExplicitMode(t *testing.T) {
	for _, test := range []struct {
		command string
		want    cleanupWorkerMode
	}{
		{command: "serve", want: cleanupWorkerModeAutomatic},
		{command: "serve-cold-quota", want: cleanupWorkerModeColdQuota},
	} {
		t.Run(test.command, func(t *testing.T) {
			mode, path, err := cleanupWorkerCommand([]string{test.command, "/mounted/cleanup.json"})
			if err != nil || mode != test.want || path != "/mounted/cleanup.json" {
				t.Fatalf("explicit mode was not retained: mode=%v path=%q error=%v", mode, path, err)
			}
		})
	}

	// Existing callers of the original helper retain its serve-only contract.
	if _, err := cleanupWorkerManifestArgument([]string{"serve-cold-quota", "/mounted/cleanup.json"}); !errors.Is(err, errCleanupWorkerCommand) {
		t.Fatalf("original serve helper admitted a new mode: %v", err)
	}
}

func TestColdQuotaCleanupWorkerCommandRefusal(t *testing.T) {
	for name, args := range map[string][]string{
		"missing-manifest": {"serve-cold-quota"},
		"empty-manifest":   {"serve-cold-quota", ""},
		"extra-argument":   {"serve-cold-quota", "secret-path", "secret-extra"},
		"unknown-mode":     {"serve-cold-quota-auto", "secret-path"},
		"case-alias":       {"Serve-cold-quota", "secret-path"},
		"nul-path":         {"serve-cold-quota", "secret\x00path"},
		"newline-path":     {"serve-cold-quota", "secret\npath"},
		"invalid-utf8":     {"serve-cold-quota", "secret\xffpath"},
		"oversize-path":    {"serve-cold-quota", strings.Repeat("s", 4097)},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := cleanupWorkerCommand(args)
			if !errors.Is(err, errCleanupWorkerCommand) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid command accepted or disclosed arguments: %v", err)
			}
		})
	}
}

func TestColdQuotaCleanupWorkerRunRefusesBeforeBootstrap(t *testing.T) {
	args := []string{"serve-cold-quota", "/missing/secret-cold-quota-manifest.json"}
	if err := run(nil, args); !errors.Is(err, errCleanupWorkerCommand) {
		t.Fatalf("nil context reached startup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, args); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context reached startup: %v", err)
	}

	var diagnostic bytes.Buffer
	err := run(ctx, []string{"serve-cold-quota", "secret\x00path"})
	if !errors.Is(err, errCleanupWorkerCommand) || cleanupWorkerExitCode(err, &diagnostic) != 1 {
		t.Fatalf("invalid cold command did not fail closed: %v", err)
	}
	if diagnostic.String() != "sandbox-cleanup-worker: failed\n" {
		t.Fatalf("cold command disclosed a private cause: %q", diagnostic.String())
	}
}
