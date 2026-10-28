package sandbox_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

// TestLiveFilesV2 is opt-in and checks the published API through its ordinary
// Service. It creates and destroys one disposable sandbox.
func TestLiveFilesV2(t *testing.T) {
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

	path := "/workspace/.codex-files-v2-deploy-smoke.bin"
	want := bytes.Repeat([]byte("streaming-file-v2\n"), 2048)
	if err := running.Files().Write(ctx, path, bytes.NewReader(want), int64(len(want)), false); err != nil {
		t.Fatal(err)
	}
	stream, err := running.Files().Read(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("file v2 read: bytes=%d read=%v close=%v equal=%t", len(got), readErr, closeErr, bytes.Equal(got, want))
	}
	t.Logf("file v2 round trip: %d bytes", len(got))
}
