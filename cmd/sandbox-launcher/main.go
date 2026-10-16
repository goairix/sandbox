package main

import (
	"context"
	"errors"
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
	case "file-download":
		return fileDownload(os.Stdin, os.Stdout, os.Stderr)
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

// Only this mode has binary stdout and a strict, application-only stderr shape.
type downloadExit struct{}

func (downloadExit) Error() string { return "file download failed" }

func fileDownload(input io.Reader, output, stderr io.Writer) error {
	wire, err := io.ReadAll(io.LimitReader(input, sandboxfiles.MaxDownloadInputBytes+1))
	var failure []byte
	if len(wire) > sandboxfiles.MaxDownloadInputBytes {
		failure = []byte("{\"code\":\"limit_exceeded\"}\n")
	} else if err != nil {
		failure = []byte("{\"code\":\"invalid_request\"}\n")
	} else {
		request, decodeErr := sandboxfiles.DecodeDownloadRequest(wire)
		if decodeErr != nil {
			failure = sandboxfiles.EncodeDownloadFailure(decodeErr)
		} else if streamErr := sandboxfiles.StreamDownload(request, output); streamErr != nil {
			failure = sandboxfiles.EncodeDownloadFailure(streamErr)
		}
	}
	if failure == nil {
		return nil
	}
	// Failed/short stderr writes still return the quiet typed exit. A caller
	// must never append a second, non-protocol diagnostic to this stream.
	_, _ = stderr.Write(failure)
	return downloadExit{}
}

func launcherExitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var quiet downloadExit
	if !errors.As(err, &quiet) {
		fmt.Fprintln(stderr, "sandbox-launcher:", err)
	}
	return 1
}

func main() {
	if code := launcherExitCode(run(os.Args[1:]), os.Stderr); code != 0 {
		os.Exit(code)
	}
}
