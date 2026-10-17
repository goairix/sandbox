package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCleanupWorkerArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"absent":           nil,
		"missing-manifest": {"serve"},
		"empty-manifest":   {"serve", ""},
		"unknown-role":     {"provision", "secret-path"},
		"extra-argument":   {"serve", "secret-path", "extra-secret"},
		"nul-path":         {"serve", "secret\x00path"},
		"oversize-path":    {"serve", strings.Repeat("s", 4097)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := cleanupWorkerManifestArgument(args)
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid command accepted or disclosed arguments: %v", err)
			}
		})
	}
	got, err := cleanupWorkerManifestArgument([]string{"serve", "/mounted/cleanup.json"})
	if err != nil || got != "/mounted/cleanup.json" {
		t.Fatalf("strict serve manifest: %q, %v", got, err)
	}
}

func TestCleanupWorkerExitRedaction(t *testing.T) {
	for name, err := range map[string]error{
		"raw":    errors.New("private-key=secret-credential"),
		"joined": errors.Join(context.Canceled, errors.New("bearer secret-credential")),
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			if cleanupWorkerExitCode(err, &output) != 1 {
				t.Fatal("failure exited successfully")
			}
			if output.String() != "sandbox-cleanup-worker: failed\n" {
				t.Fatalf("failure diagnostic disclosed a cause: %q", output.String())
			}
		})
	}
	var output bytes.Buffer
	if cleanupWorkerExitCode(nil, &output) != 0 || output.Len() != 0 {
		t.Fatal("successful shutdown emitted a failure")
	}
}
