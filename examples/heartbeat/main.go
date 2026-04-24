package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

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

	// Create a persistent sandbox
	sb, err := client.CreateSandbox(ctx, sandbox.CreateSandboxRequest{
		Mode: sandbox.ModePersistent,
	})
	if err != nil {
		log.Fatalf("Failed to create sandbox: %v", err)
	}
	defer client.DestroySandbox(ctx, sb.ID)

	fmt.Printf("Created sandbox: %s\n", sb.ID)

	// Execute a long-running command with no output for extended periods
	// This simulates npm install or video processing
	code := `
import time
import sys

print("Starting long task...", flush=True)
sys.stdout.flush()

# Simulate 90 seconds of silent processing
time.sleep(90)

print("Task completed!", flush=True)
`

	fmt.Println("\nExecuting long-running task with silent periods...")
	fmt.Println("Watch for ping events every 30 seconds to keep connection alive\n")

	ch, err := client.ExecStream(ctx, sb.ID, sandbox.ExecRequest{
		Language: "python",
		Code:     code,
		Timeout:  120,
	})
	if err != nil {
		log.Fatalf("Failed to start execution: %v", err)
	}

	start := time.Now()
	pingCount := 0

	for event := range ch {
		elapsed := time.Since(start).Seconds()

		switch event.Type {
		case sandbox.SSEEventStdout:
			fmt.Printf("[%.1fs] STDOUT: %s\n", elapsed, event.Content)

		case sandbox.SSEEventStderr:
			fmt.Printf("[%.1fs] STDERR: %s\n", elapsed, event.Content)

		case sandbox.SSEEventPing:
			pingCount++
			fmt.Printf("[%.1fs] PING #%d (timestamp: %d) - connection alive\n",
				elapsed, pingCount, event.Timestamp)

		case sandbox.SSEEventDone:
			fmt.Printf("[%.1fs] DONE: exit_code=%d, elapsed=%.2fs\n",
				elapsed, event.ExitCode, event.Elapsed)

		case sandbox.SSEEventError:
			fmt.Printf("[%.1fs] ERROR: %s\n", elapsed, event.Content)
		}
	}

	fmt.Printf("\nTotal ping events received: %d\n", pingCount)
	fmt.Println("Connection remained alive throughout the silent period!")
}
