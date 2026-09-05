//go:build !linux

package mounter

import (
	"fmt"
	"net"
)

func defaultVerifyRootPeer(*net.UnixConn) error {
	return fmt.Errorf("root peer credential verification is unavailable")
}
