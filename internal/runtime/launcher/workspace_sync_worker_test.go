package launcher

import (
	"errors"
	"reflect"
	"runtime"
	"testing"
)

func workspaceSyncWorkerModel() KernelSnapshot {
	return KernelSnapshot{PID: 1, UIDs: [4]uint32{65532, 65532, 65532, 65532}, GIDs: [4]uint32{65532, 65532, 65532, 65532}, CapLast: 40, Threads: 4, NoNewPrivileges: true, Subreaper: true}
}

func TestWorkspaceSyncWorkerModel(t *testing.T) {
	for _, initial := range []bool{true, false} {
		if err := validateKernelSnapshot(workspaceSyncWorkerModel(), roleWorkspaceSyncWorker, initial); err != nil {
			t.Fatalf("compliant non-root zero-cap worker rejected: %v", err)
		}
	}
	for name, change := range map[string]func(*KernelSnapshot){
		"pid-zero":          func(s *KernelSnapshot) { s.PID = 0 },
		"uid-zero":          func(s *KernelSnapshot) { s.UIDs = [4]uint32{} },
		"gid-zero":          func(s *KernelSnapshot) { s.GIDs = [4]uint32{} },
		"uid-nonuniform":    func(s *KernelSnapshot) { s.UIDs[3]++ },
		"gid-nonuniform":    func(s *KernelSnapshot) { s.GIDs[2]++ },
		"permitted":         func(s *KernelSnapshot) { s.Permitted = 1 },
		"effective":         func(s *KernelSnapshot) { s.Effective = 1 },
		"inheritable":       func(s *KernelSnapshot) { s.Inheritable = 1 },
		"ambient":           func(s *KernelSnapshot) { s.Ambient = 1 },
		"bounding":          func(s *KernelSnapshot) { s.Bounding = 1 },
		"nnp":               func(s *KernelSnapshot) { s.NoNewPrivileges = false },
		"securebits":        func(s *KernelSnapshot) { s.Securebits = 1 },
		"dumpable-negative": func(s *KernelSnapshot) { s.Dumpable = -1 },
		"dumpable-extra":    func(s *KernelSnapshot) { s.Dumpable = 2 },
		"dumpable-final":    func(s *KernelSnapshot) { s.Dumpable = 1 },
		"subreaper":         func(s *KernelSnapshot) { s.Subreaper = false },
		"cap-low":           func(s *KernelSnapshot) { s.CapLast = 7 },
		"cap-high":          func(s *KernelSnapshot) { s.CapLast = 64 },
		"thread-zero":       func(s *KernelSnapshot) { s.Threads = 0 },
		"thread-high":       func(s *KernelSnapshot) { s.Threads = 4097 },
	} {
		t.Run(name, func(t *testing.T) {
			s := workspaceSyncWorkerModel()
			change(&s)
			if err := validateKernelSnapshot(s, roleWorkspaceSyncWorker, false); !errors.Is(err, ErrUnsafeKernel) {
				t.Fatalf("unsafe worker accepted: %v", err)
			}
		})
	}
	s := workspaceSyncWorkerModel()
	s.PID, s.Dumpable, s.Subreaper = 2, 1, false
	if err := validateKernelSnapshot(s, roleWorkspaceSyncWorker, true); err != nil {
		t.Fatal(err)
	}
	for _, role := range []kernelRole{rolePID1, roleMonitor, 99} {
		if validateKernelSnapshot(workspaceSyncWorkerModel(), role, false) == nil {
			t.Fatal("worker accepted by foreign role")
		}
	}
}

func workspaceSyncWorkerReferenceModel() WorkspaceSyncWorkerReference {
	return WorkspaceSyncWorkerReference{Incarnation: "8a7a11c2-9199-4dc9-a5f7-f388a9107745", BootID: "86b3c998-f1b4-44b6-8797-dfba528adbb5", PID: 1, StartTicks: 10, UID: 65532, GID: 65532, PIDNamespaceDev: 4, PIDNamespaceIno: 5, MountNamespaceDev: 4, MountNamespaceIno: 6, ExecutableDev: 7, ExecutableIno: 8}
}

func TestWorkspaceSyncWorkerReferenceStructure(t *testing.T) {
	r := workspaceSyncWorkerReferenceModel()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	// Every field is mandatory, including all independent namespace/exe pins.
	typ := reflect.TypeOf(r)
	for i := 0; i < typ.NumField(); i++ {
		t.Run(typ.Field(i).Name, func(t *testing.T) {
			bad := r
			v := reflect.ValueOf(&bad).Elem().Field(i)
			v.Set(reflect.Zero(v.Type()))
			if bad.Validate() == nil {
				t.Fatal("zero structural field accepted")
			}
		})
	}
	for _, bad := range []string{"not-uuid", "8A7A11C2-9199-4DC9-A5F7-F388A9107745", "8a7a11c291994dc9a5f7f388a9107745", "00000000-0000-0000-0000-000000000000", "8a7a11c2-9199-1dc9-a5f7-f388a9107745"} {
		r.Incarnation = bad
		if r.Validate() == nil {
			t.Fatalf("bad incarnation accepted: %q", bad)
		}
	}
	r = workspaceSyncWorkerReferenceModel()
	r.PID = -1
	if r.Validate() == nil {
		t.Fatal("negative PID accepted")
	}
}

func TestWorkspaceSyncWorkerOriginalDiagnostics(t *testing.T) {
	r := workspaceSyncWorkerReferenceModel()
	b := &KernelBoundary{role: roleWorkspaceSyncWorker, pid: 1, verified: workspaceSyncWorkerModel(), workerSeal: &workspaceSyncWorkerSeal{reference: r}}
	b.self = b
	got, err := b.WorkspaceSyncWorkerReference()
	if err != nil || got != r {
		t.Fatalf("reference: %+v %v", got, err)
	}
	got.UID++
	if again, err := b.WorkspaceSyncWorkerReference(); err != nil || again != r {
		t.Fatal("reference was not copied")
	}
	snapshot := b.Snapshot()
	snapshot.UIDs[0]++
	if b.Snapshot() != workspaceSyncWorkerModel() {
		t.Fatal("snapshot was not copied")
	}
	copyBoundary := &KernelBoundary{self: b, role: b.role, pid: b.pid, verified: b.verified, workerSeal: b.workerSeal}
	for _, bad := range []*KernelBoundary{nil, {}, copyBoundary} {
		if ref, err := bad.WorkspaceSyncWorkerReference(); err == nil || ref != (WorkspaceSyncWorkerReference{}) {
			t.Fatal("nonoriginal boundary returned reference")
		}
	}
	for _, role := range []kernelRole{rolePID1, roleMonitor, 0} {
		b.role = role
		if _, err := b.WorkspaceSyncWorkerReference(); err == nil {
			t.Fatal("foreign role returned reference")
		}
	}
	b.role = roleWorkspaceSyncWorker
	for _, drift := range []string{"UID", "GID"} {
		s := workspaceSyncWorkerModel()
		if drift == "UID" {
			s.UIDs = [4]uint32{65531, 65531, 65531, 65531}
		} else {
			s.GIDs = [4]uint32{65531, 65531, 65531, 65531}
		}
		if validateKernelSnapshot(s, roleWorkspaceSyncWorker, false) != nil {
			t.Fatal("alternate worker model invalid")
		}
		if b.workerSeal.matchesSnapshot(s) {
			t.Fatal("original worker accepts nonzero credential drift")
		}
		b.verified = s
		if _, err := b.WorkspaceSyncWorkerReference(); err == nil {
			t.Fatal("drifted boundary reference accepted")
		}
	}
}

func TestWorkspaceSyncWorkerUnsupported(t *testing.T) {
	if runtime.GOOS == "linux" {
		return
	}
	if b, err := BootstrapWorkspaceSyncWorker(); b != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported host bootstrap: %v %v", b, err)
	}
}
