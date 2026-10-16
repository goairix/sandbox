//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"fmt"
	"os"
	"testing"
)

func TestBusinessSnapshotColdServiceNative(t *testing.T) {
	if os.Getenv("SANDBOX_COLD_CREATE_SERVICE_NATIVE") != "1" {
		t.Skip("requires exact owned cold-create service Linux fixture")
	}
	for _, loseACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("activation-ACK-lost-%t", loseACK), func(t *testing.T) {
			runColdCreateServiceBusinessNative(t, loseACK, true)
		})
	}
	t.Run("v1-compatibility", func(t *testing.T) {
		runColdCreateServiceNative(t, false)
	})
}
