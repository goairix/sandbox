package main

import (
	"context"
	"io"
	"testing"
)

func TestRunCLIRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"--check-interval=0"}, {"--profile-path=relative"}, {"readiness", "--max-age=0"}, {"readiness", "--readiness-file=/nonexistent"}, {"unexpected-tenant-input"}} {
		if err := run(context.Background(), args, io.Discard); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
