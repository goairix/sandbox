package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	sandbox "github.com/goairix/sandbox/sdk/go"
)

func main() {
	apiKey := os.Getenv("SANDBOX_API_KEY")
	if apiKey == "" {
		log.Fatal("SANDBOX_API_KEY environment variable is required")
	}

	baseURL := os.Getenv("SANDBOX_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}

	client := sandbox.NewClient(baseURL, apiKey)
	ctx := context.Background()

	// Create a sandbox and write a sample file
	sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
		Mode: sandbox.ModeEphemeral,
	})
	if err != nil {
		log.Fatalf("Failed to create sandbox: %v", err)
	}
	defer client.DestroySandbox(ctx, sb.ID)

	fmt.Printf("Created sandbox: %s\n", sb.ID)

	// Generate a sample file inside the sandbox
	_, err = client.Exec(ctx, sb.ID, sandbox.ExecRequest{
		Language: "python",
		Code: `
with open("/workspace/hello.txt", "w") as f:
    for i in range(1, 6):
        f.write(f"line {i}: hello from sandbox\n")
print("file written")
`,
	})
	if err != nil {
		log.Fatalf("Failed to write file: %v", err)
	}

	// Read the file as a stream — no buffering, works for any file size
	reader, err := client.ReadFile(ctx, sb.ID, "/workspace/hello.txt")
	if err != nil {
		log.Fatalf("Failed to read file: %v", err)
	}
	defer reader.Close()

	fmt.Println("\n--- file content ---")

	// Stream directly to stdout
	if _, err := io.Copy(os.Stdout, reader); err != nil {
		log.Fatalf("Failed to stream file: %v", err)
	}

	fmt.Println("--- end ---")

	// Example: stream to a local file instead
	reader2, err := client.ReadFile(ctx, sb.ID, "/workspace/hello.txt")
	if err != nil {
		log.Fatalf("Failed to read file: %v", err)
	}
	defer reader2.Close()

	out, err := os.Create("/tmp/hello.txt")
	if err != nil {
		log.Fatalf("Failed to create local file: %v", err)
	}
	defer out.Close()

	n, err := io.Copy(out, reader2)
	if err != nil {
		log.Fatalf("Failed to save file: %v", err)
	}

	fmt.Printf("\nSaved %d bytes to /tmp/hello.txt\n", n)
}
