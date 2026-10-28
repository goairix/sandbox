package sandbox_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

// TestLiveExecutionStream is opt-in and uses one disposable sandbox. It checks
// real HTTP chunk timing and Kubernetes process exit semantics without changing
// a shared API Deployment.
func TestLiveExecutionStream(t *testing.T) {
	baseURL, apiKey := os.Getenv("SANDBOX_STREAM_LIVE_BASE_URL"), os.Getenv("SANDBOX_STREAM_LIVE_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("set SANDBOX_STREAM_LIVE_BASE_URL and SANDBOX_STREAM_LIVE_API_KEY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client := sandbox.NewClient(baseURL, apiKey)
	running, err := client.NewSandbox(ctx, sandbox.SandboxOptions{Mode: sandbox.ModePersistent, Timeout: 300})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("test sandbox: %s", running.ID())
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := running.Close(cleanupCtx); err != nil {
			t.Errorf("destroy test sandbox: %v", err)
		}
	}()

	stream, err := client.ExecStream(ctx, running.ID(), sandbox.ExecRequest{
		Language: "bash", Code: "printf 'first\\n'; sleep 2; printf 'second\\n' >&2; exit 7", Timeout: 15,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var firstAt, secondAt time.Time
	var exitCode int
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, io.EOF) {
				t.Fatalf("stream receive: %v", recvErr)
			}
			break
		}
		switch event.Type {
		case sandbox.SSEEventStdout:
			if strings.Contains(event.Content, "first") {
				firstAt = time.Now()
				t.Logf("stdout first chunk at %s", firstAt.Format(time.RFC3339Nano))
			}
		case sandbox.SSEEventStderr:
			if strings.Contains(event.Content, "second") {
				secondAt = time.Now()
				t.Logf("stderr second chunk at %s", secondAt.Format(time.RFC3339Nano))
			}
		case sandbox.SSEEventDone:
			exitCode = event.ExitCode
			t.Logf("done exit_code=%d", exitCode)
		case sandbox.SSEEventError:
			t.Fatalf("execution error event: %s", event.Content)
		}
	}
	if firstAt.IsZero() || secondAt.IsZero() || secondAt.Sub(firstAt) < 1500*time.Millisecond || exitCode != 7 {
		t.Fatalf("not a live two-chunk nonzero execution: first=%v second=%v exit=%d", firstAt, secondAt, exitCode)
	}

	silentCtx, silentCancel := context.WithCancel(ctx)
	started := time.Now()
	silent, err := client.ExecStream(silentCtx, running.ID(), sandbox.ExecRequest{
		Language: "bash", Code: "sleep 30", Timeout: 35,
	})
	if err != nil {
		silentCancel()
		t.Fatal(err)
	}
	openLatency := time.Since(started)
	t.Logf("silent command stream opened after %s", openLatency)
	silentCancel()
	if err := silent.Close(); err != nil {
		t.Fatal(err)
	}
	if openLatency > 2*time.Second {
		t.Fatalf("silent stream waited for output or heartbeat: %s", openLatency)
	}
	// Closing the response must release the sandbox operation gate so a new
	// execution can proceed. The remote process lifecycle is handled by the
	// Kubernetes exec session and sandbox teardown.
	resumeCtx, resumeCancel := context.WithTimeout(ctx, 5*time.Second)
	defer resumeCancel()
	for {
		result, runErr := client.Exec(resumeCtx, running.ID(), sandbox.ExecRequest{Language: "bash", Code: "printf resumed"})
		if runErr == nil {
			if result.Stdout != "resumed" || result.ExitCode != 0 {
				t.Fatalf("post-close execution=%+v", result)
			}
			t.Log("post-close execution succeeded")
			break
		}
		var apiErr *sandbox.SandboxError
		if !errors.As(runErr, &apiErr) || apiErr.StatusCode != 409 {
			t.Fatalf("post-close execution failed: %v", runErr)
		}
		if resumeCtx.Err() != nil {
			t.Fatalf("stream Close did not release sandbox execution: %v", runErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
