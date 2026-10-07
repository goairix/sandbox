package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
)

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected fixed serve, monitor or bridge mode")
	}
	switch args[0] {
	case "serve":
		return serve()
	case "monitor":
		return monitor()
	case "bridge":
		return controltransport.Bridge(context.Background(), os.Stdin, os.Stdout)
	default:
		return fmt.Errorf("unknown fixed mode")
	}
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-launcher:", err)
		os.Exit(1)
	}
}
