//go:build !linux || (!amd64 && !arm64)

package nodeobserver

import (
	"context"
	"fmt"
	"net"
)

type Observer struct{}

func New(Options) (*Observer, error) {
	return nil, fmt.Errorf("node observer requires Linux amd64/arm64 pidfd and cgroup-v2")
}

func (*Observer) Serve(_ context.Context, enroll *net.UnixListener, commands net.Listener) error {
	if enroll != nil {
		_ = enroll.Close()
	}
	if commands != nil {
		_ = commands.Close()
	}
	return fmt.Errorf("unsupported node observer kernel")
}

func (*Observer) Close() error { return nil }
