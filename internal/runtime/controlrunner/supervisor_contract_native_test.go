//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"testing"
)

func TestTask3NativeBirthAndAdmission(t *testing.T) {
	f := newNativeFixture(t)
	birth := f.s.Birth()
	birth.RuntimePublicKey[0] ^= 1
	if f.s.Birth().RuntimePublicKey[0] == birth.RuntimePublicKey[0] {
		t.Fatal("birth aliases private state")
	}
	for _, mutate := range []func(*SupervisorOptions){func(o *SupervisorOptions) { o.Kernel = nil }, func(o *SupervisorOptions) { o.Kernel = &launcher.KernelBoundary{} }, func(o *SupervisorOptions) { o.UID = 0 }, func(o *SupervisorOptions) { o.GID = 0 }, func(o *SupervisorOptions) { o.MaxActive = 65 }, func(o *SupervisorOptions) { o.Executable = "/tmp" }} {
		o := f.s.options
		mutate(&o)
		if s, e := NewSupervisor(o); e == nil || s != nil {
			t.Fatal("unsafe construction accepted")
		}
	}
	fresh, err := NewSupervisor(f.s.options)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.Birth().BootID == f.s.Birth().BootID {
		t.Fatal("birth reused")
	}
	if err = fresh.Activate(context.Background(), *f.s.activation); err == nil {
		t.Fatal("historic activation reconstructed new birth")
	}
	if _, err = fresh.accept(context.Background(), &authenticatedStart{supervisor: fresh}); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatal("inactive start reached admission", err)
	}
	for _, network := range []bool{false, true} {
		r := controlprotocol.ExecutionRequest{Argv: []string{"/bad"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 1, RequiresNetwork: network}
		if !network {
			r.UID = 1001
		}
		d, e := controlprotocol.NewExecutionDescriptor(r)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.s.accept(context.Background(), &authenticatedStart{supervisor: f.s, descriptor: d}); !errors.Is(e, ErrInvalidConfiguration) {
			t.Fatal("activated UID/network guard missed", e)
		}
	}
	if f.s.journal.Status().Records != 0 {
		t.Fatal("rejected requests persisted start")
	}
	t.Log("actual PID1 rejects zero boundary/UID/GID/oversized registry/unsafe executable; fresh boot/key rejects old activation; inactive and UID/network mismatch cannot accept")
}
