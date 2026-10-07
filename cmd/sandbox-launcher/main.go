package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "monitor" {
		fmt.Fprintln(os.Stderr, "sandbox-launcher: expected fixed monitor mode")
		os.Exit(2)
	}
	if err := monitor(); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox-launcher monitor:", err)
		os.Exit(1)
	}
}
