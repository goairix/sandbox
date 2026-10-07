//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestPreparedUserStartRejectsSetupDelay(t *testing.T) {
	for _, scenario := range []string{"authority_expires_during_setup", "command_expires_during_setup", "command_canceled_after_setup"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			initial, err := monitorMonotonic()
			if err != nil {
				t.Fatal(err)
			}
			authority, command := initial+int64(4*time.Second), initial+int64(4*time.Second)
			expires := initial + int64(200*time.Millisecond)
			if scenario == "authority_expires_during_setup" {
				authority = expires
			}
			if scenario == "command_expires_during_setup" {
				command = expires
			}
			checked, err := monitorMonotonic()
			if err != nil || checked >= authority || checked >= command || ctx.Err() != nil {
				t.Fatalf("window was not fresh before preparation: %v", err)
			}
			if scenario == "command_canceled_after_setup" {
				cancel()
			} else {
				// An absolute clock barrier, not a presumed relative sleep: preparation has
				// definitely consumed the formerly valid window before the boundary call.
				for {
					now, err := monitorMonotonic()
					if err != nil {
						t.Fatal(err)
					}
					if now >= expires {
						break
					}
					if ctx.Err() != nil {
						t.Fatal("clock barrier exceeded bound")
					}
					time.Sleep(time.Millisecond)
				}
			}
			unexpectedStart := errors.New("exec.Cmd.Start boundary was invoked")
			// exec.Cmd checks Err upon Start. This deterministic sentinel detects the
			// real method invocation without a spawn callback or creating any process.
			cmd := &exec.Cmd{Path: "/unreachable-test-only", Err: unexpectedStart}
			err = startPreparedUser(ctx, authority, command, cmd)
			if errors.Is(err, unexpectedStart) {
				t.Fatal("expired/canceled setup crossed the Start boundary")
			}
			if err == nil || cmd.Process != nil {
				t.Fatalf("expired/canceled setup accepted: process=%v err=%v", cmd.Process, err)
			}
			if scenario != "command_canceled_after_setup" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expiry lost: %v", err)
			}
			if scenario == "command_canceled_after_setup" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}
