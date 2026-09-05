//go:build !linux

package mounter

import (
	"context"
	"fmt"
	"net"
)

func dockerReaperPeerIdentity(net.Conn) (int, int, error) {
	return 0, 0, fmt.Errorf("Docker reaper requires Linux")
}
func dockerReaperProcessIdentity(int) (reaperProcessIdentity, error) {
	return reaperProcessIdentity{}, fmt.Errorf("Docker reaper requires Linux")
}
func dockerWaitExactChild(context.Context, reaperProcessIdentity) error {
	return fmt.Errorf("Docker reaper requires Linux")
}
