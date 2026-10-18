//go:build !linux || (!amd64 && !arm64)

package nodeobserver

import (
	"context"
	"fmt"
	"net"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
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

// ServeWithMounter cannot authenticate native process anchors on this kernel.
func (o *Observer) ServeWithMounter(ctx context.Context, enroll *net.UnixListener, commands net.Listener, mounter *net.UnixListener) error {
	if mounter != nil {
		_ = mounter.Close()
	}
	return o.Serve(ctx, enroll, commands)
}

// ReadMounterReadiness returns no positive observation without native support.
func (*Observer) ReadMounterReadiness(context.Context, p.RuntimeIdentityContext, string, int64, uint8) (fuseprotocol.MounterReadiness, error) {
	return fuseprotocol.MounterReadiness{}, fmt.Errorf("unsupported node observer kernel")
}
