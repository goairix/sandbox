package main

import (
	"os"

	"github.com/goairix/sandbox/internal/workspaceprobe"
)

func main() {
	if pid1, code := workspaceprobe.MaybeRunPID1(); pid1 {
		os.Exit(code)
	}
	if broker, code := workspaceprobe.MaybeRunBroker(); broker {
		os.Exit(code)
	}
	if helper, code := workspaceprobe.MaybeRunIOHelper(); helper {
		os.Exit(code)
	}
	os.Exit(workspaceprobe.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
