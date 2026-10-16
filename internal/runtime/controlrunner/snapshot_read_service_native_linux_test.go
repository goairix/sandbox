//go:build linux && (amd64 || arm64)

package controlrunner

import "testing"

// Both original success paths begin with fresh intent acquisition. Ordinary
// Create and activation-ACK Resume therefore exercise the ongoing service's
// coherent snapshot read before original native publication and settlement.
func TestSnapshotReadColdServiceNative(t *testing.T) {
	TestColdCreateServiceNative(t)
}
