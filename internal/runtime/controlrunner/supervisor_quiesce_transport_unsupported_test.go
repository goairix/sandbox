//go:build !linux || (!amd64 && !arm64)

package controlrunner

import "testing"

func TestTaskQuiesceTransportNativeTerminalBeforeACK(t *testing.T) {
	t.Skip("requires controller-owned supported Linux protected PID1; host is not physical proof")
}
