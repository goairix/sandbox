//go:build !linux

package workspaceprobe

import (
	"fmt"
	"net"
)

func unixPeerIdentity(net.Conn) (int, int, error) {
	return 0, 0, fmt.Errorf("unix peer credentials are supported only on linux")
}
