//go:build linux && (amd64 || arm64)

package launcher

import (
	"context"
	"os"
	"testing"
)

func TestPID1QuiescenceNativeSeal(t *testing.T) {
	if os.Getenv("SANDBOX_TASK3_NATIVE") != "1" {
		t.Skip("requires Root-owned protected actual PID1")
	}
	b, err := BootstrapPID1()
	if err != nil {
		t.Fatal(err)
	}
	original := *b.seal
	o, err := b.ObserveNoUserDescendants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual PID=%d kernel=%+v bootstrap-seal=%+v; only PID1 census and ECHILD", os.Getpid(), b.Snapshot(), original)
	copyObservation := *o
	if err = copyObservation.RevalidateCurrent(context.Background()); err == nil {
		t.Fatal("copied observation accepted")
	}
	t.Run("StrictParsers", TestPID1QuiescenceSealParsing)
	mutations := map[string]func(*pid1Seal){
		"proc-device": func(s *pid1Seal) { s.procDev++ }, "proc-inode": func(s *pid1Seal) { s.procIno++ }, "namespace-device": func(s *pid1Seal) { s.namespaceDev++ }, "namespace-inode": func(s *pid1Seal) { s.namespaceIno++ }, "proc-mount": func(s *pid1Seal) { s.procMount += " changed" }, "cgroup-device": func(s *pid1Seal) { s.cgroupDev++ }, "cgroup-inode": func(s *pid1Seal) { s.cgroupIno++ }, "cgroup-mount": func(s *pid1Seal) { s.cgroupMount += " changed" }, "membership": func(s *pid1Seal) { s.membership = "0::/changed\n" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			b.mu.Lock()
			mutate(b.seal)
			b.mu.Unlock()
			defer func() { b.mu.Lock(); *b.seal = original; b.mu.Unlock() }()
			if err := b.ValidateCurrent(); err == nil {
				t.Fatal("saved seal mismatch accepted against actual unchanged kernel")
			}
			if observed, err := b.ObserveNoUserDescendants(context.Background()); err == nil || observed != nil {
				t.Fatal("mismatched original seal minted observation")
			}
		})
	}
	if err = o.RevalidateCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log("all original-seal component sensitivity checks restored; no namespace/mount/cgroup replacement was performed")
}
