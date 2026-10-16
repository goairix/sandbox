package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/sandboxfiles"
)

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected fixed serve, serve-provisioned, monitor, bridge or file-operation mode")
	}
	switch args[0] {
	case "serve":
		return serve()
	case "serve-provisioned":
		return serveProvisioned()
	case "monitor":
		return monitor()
	case "bridge":
		return controltransport.Bridge(context.Background(), os.Stdin, os.Stdout)
	case "file-operation":
		return fileOperation(os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown fixed mode")
	}
}

func fileOperation(input io.Reader, output io.Writer) error {
	if err := sandboxfiles.Serve(input, output); err != nil {
		return fmt.Errorf("file operation transport failed")
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-launcher:", err)
		os.Exit(1)
	}
}
