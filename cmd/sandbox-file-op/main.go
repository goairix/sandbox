package main

import (
	"context"
	"fmt"
	"os"

	"github.com/goairix/sandbox/internal/filev2"
)

func main() {
	if err := filev2.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(filev2.ExitStatus(err))
	}
}
