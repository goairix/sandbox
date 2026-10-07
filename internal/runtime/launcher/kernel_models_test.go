package launcher

import (
	"errors"
	"fmt"
	"testing"
)

func TestKernelModels(t *testing.T) {
	// Literal fixtures catch changes to the privilege contract independently of its implementation.
	fixtures := []struct {
		name    string
		role    kernelRole
		initial bool
		s       KernelSnapshot
	}{
		{"pid1-initial", 1, true, KernelSnapshot{PID: 1, Permitted: 0x1e0, Effective: 0x1e0, Bounding: 0x1e0, CapLast: 40, NoNewPrivileges: true, Dumpable: 1, Threads: 4}},
		{"pid1-final", 1, false, KernelSnapshot{PID: 1, Permitted: 0xe0, Effective: 0xe0, Inheritable: 0xe0, CapLast: 40, NoNewPrivileges: true, Subreaper: true, Threads: 4}},
		{"monitor-initial", 2, true, KernelSnapshot{PID: 12, Permitted: 0xe0, Effective: 0xe0, Inheritable: 0xe0, CapLast: 40, NoNewPrivileges: true, Dumpable: 1, Threads: 4}},
		{"monitor-final", 2, false, KernelSnapshot{PID: 12, Permitted: 0xe0, Effective: 0xe0, CapLast: 40, NoNewPrivileges: true, Subreaper: true, Threads: 4}},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			if err := validateKernelSnapshot(f.s, f.role, f.initial); err != nil {
				t.Fatal(err)
			}
			reject := func(name string, mutate func(*KernelSnapshot)) {
				t.Run(name, func(t *testing.T) {
					s := f.s
					mutate(&s)
					if err := validateKernelSnapshot(s, f.role, f.initial); !errors.Is(err, ErrUnsafeKernel) {
						t.Fatalf("unsafe state accepted: %+v, err=%v", s, err)
					}
				})
			}
			for i := 0; i < 4; i++ {
				reject(fmt.Sprintf("uid%d", i), func(s *KernelSnapshot) { s.UIDs[i] = 1000 })
				reject(fmt.Sprintf("gid%d", i), func(s *KernelSnapshot) { s.GIDs[i] = 1000 })
			}
			for set := 0; set < 5; set++ {
				for bit := uint(0); bit < 64; bit++ {
					reject(fmt.Sprintf("cap%d-bit%d", set, bit), func(s *KernelSnapshot) {
						p := []*uint64{&s.Permitted, &s.Effective, &s.Inheritable, &s.Ambient, &s.Bounding}
						*p[set] ^= 1 << bit
					})
				}
			}
			reject("nnp", func(s *KernelSnapshot) { s.NoNewPrivileges = false })
			reject("securebits", func(s *KernelSnapshot) { s.Securebits = 1 })
			reject("dumpable-negative", func(s *KernelSnapshot) { s.Dumpable = -1 })
			reject("dumpable-two", func(s *KernelSnapshot) { s.Dumpable = 2 })
			reject("pid-zero", func(s *KernelSnapshot) { s.PID = 0 })
			reject("pid-negative", func(s *KernelSnapshot) { s.PID = -1 })
			reject("wrong-role-pid", func(s *KernelSnapshot) {
				if f.role == 1 {
					s.PID = 2
				} else {
					s.PID = 1
				}
			})
			reject("caplast-low", func(s *KernelSnapshot) { s.CapLast = 7 })
			reject("caplast-high", func(s *KernelSnapshot) { s.CapLast = 64 })
			reject("threads-zero", func(s *KernelSnapshot) { s.Threads = 0 })
			reject("threads-overflow", func(s *KernelSnapshot) { s.Threads = 4097 })
			if !f.initial {
				reject("dumpable-one", func(s *KernelSnapshot) { s.Dumpable = 1 })
				reject("subreaper", func(s *KernelSnapshot) { s.Subreaper = false })
			}
			for _, n := range []uint32{1, 4096} {
				s := f.s
				s.Threads = n
				if err := validateKernelSnapshot(s, f.role, f.initial); err != nil {
					t.Fatal(err)
				}
			}
			for _, n := range []uint32{8, 63} {
				s := f.s
				s.CapLast = n
				if err := validateKernelSnapshot(s, f.role, f.initial); err != nil {
					t.Fatal(err)
				}
			}
			if f.initial {
				s := f.s
				s.Dumpable = 0
				s.Subreaper = true
				if err := validateKernelSnapshot(s, f.role, true); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if err := validateKernelSnapshot(KernelSnapshot{}, 1, true); !errors.Is(err, ErrUnsafeKernel) {
		t.Fatalf("zero accepted: %v", err)
	}
	for _, role := range []kernelRole{0, 3, 255} {
		if err := validateKernelSnapshot(fixtures[0].s, role, true); !errors.Is(err, ErrUnsafeKernel) {
			t.Fatalf("bad role accepted: %d %v", role, err)
		}
	}
}

func TestKernelHandles(t *testing.T) {
	for _, b := range []*KernelBoundary{nil, {}} {
		if s := b.Snapshot(); s != (KernelSnapshot{}) {
			t.Fatalf("empty handle snapshot=%+v", s)
		}
		if err := b.ValidateCurrent(); !errors.Is(err, ErrKernelUnavailable) {
			t.Fatalf("empty handle validate=%v", err)
		}
	}
	b := &KernelBoundary{verified: KernelSnapshot{PID: 1, Threads: 4}}
	b.self = b
	s := b.Snapshot()
	s.Threads = 77
	if b.Snapshot().Threads != 4 {
		t.Fatal("snapshot aliases handle")
	}
}
