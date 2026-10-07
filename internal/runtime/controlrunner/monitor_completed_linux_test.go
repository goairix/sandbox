//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"testing"
)

// Closed-before-readiness is independently forced by RootLifecycleReadiness.
// These model observations exercise the exact historical-result predicate used
// after that rejected live readiness; they are not native wait/drain evidence.
func TestCompletedRootRegistrationModel(t *testing.T) {
	valid := launcher.RootExitObservation{RootPID: 42, RootWaitStatus: 0, Drain: launcher.LocalDrainObservation{MonitorPID: 12}, Reason: "root_exit"}
	if !validCompletedRoot(valid, 42, 12) {
		t.Fatal("attributable completed root discarded")
	}
	for _, change := range []func(*launcher.RootExitObservation){
		func(o *launcher.RootExitObservation) { o.RootPID = 43 },
		func(o *launcher.RootExitObservation) { o.Drain.MonitorPID = 13 },
		func(o *launcher.RootExitObservation) { o.RootWaitStatus = 0x7f },
	} {
		o := valid
		change(&o)
		if validCompletedRoot(o, 42, 12) {
			t.Fatalf("invalid historical result accepted %+v", o)
		}
	}
	if validCompletedRoot(launcher.RootExitObservation{}, 42, 12) {
		t.Fatal("missing original worker outcome accepted")
	}
}
