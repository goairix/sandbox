# Monitor confinement and local descendant drain

## Intent and scope

This implements the next physical prerequisite of the approved native etcd migration, after the protected runtime journal and launcher kernel boundary. The user authorized continued implementation and small verified commits. Large existing idle sandbox count N must not create per-sandbox control-plane leases, RPC polling, or descendant scans. Work here occurs only during monitor startup or active command cleanup.

One trusted root monitor owns one command inside the sandbox's existing PID namespace and resource cgroup. Before starting user code it is a subreaper, has the previously verified monitor kernel boundary, and installs an inherited namespace/ptrace restriction. A cleanup handle can kill and reap its own user children. It never authorizes execution, authenticates activation, opens a journal gate, or proves remote writes have settled. Production Manager, images, FUSE topology, Redis configuration and raw adapters remain for subsequent integration.

## Design choices

Use a per-command monitor rather than attribution by a single PID1 or process group. Double forks and setsid can escape a process group; a live nearest subreaper retains the command ancestry. Avoid a retained transitive PID tree: repeatedly kill only direct children of this monitor through pinned proc-directory handles, then reap and repeat as descendants are adopted. This reduces PID-reuse and inconsistent ancestor-snapshot risk. Do not rely on SIGSTOP: same-UID commands may send SIGCONT.

Do not use delegated writable cgroups or introduce SYS_ADMIN/SYS_PTRACE. An inherited seccomp layer denies namespace creation and process tracing; actual cgroup mounts must be read-only. This is a confinement component, not a complete sandbox security policy. Existing runtime isolation, protected descriptors and the later trusted target transport remain necessary.

## Interfaces and ownership

Extend `internal/runtime/launcher` with:

```go
func ConfineMonitor(kernel *KernelBoundary, uid, gid uint32) (*MonitorBoundary, error)
func (m *MonitorBoundary) ValidateCurrent() error
func (m *MonitorBoundary) Drain(ctx context.Context) (LocalDrainObservation, error)

type ProcessExit struct {
    PID int
    WaitStatus uint32
}
type LocalDrainObservation struct {
    MonitorPID int
    Reaped []ProcessExit
}
```

`MonitorBoundary` is opaque, process-local, non-copyable, and cannot be reconstructed from diagnostic output. The constructor sets a private self pointer; every method requires self==receiver, so even a same-PID struct copy cannot create another wait owner or bypass poison with a copied mutex. Nil/zero/copied/another-PID handles fail closed. It seals a real current monitor-role kernel boundary and one UID/GID, each `1..2147483647`. PID1/mounter boundaries are rejected. One confinement attempt per process is consumed before valid-role preflight; ordinary failure returns no handle and requires caller termination, and divergent all-thread setter outcomes remain Go runtime fatal. Invalid nil/zero/wrong-role inputs fail without mutating the kernel.

Before confinement there must be no child process: non-consuming `waitid(P_ALL, WEXITED|WNOHANG|WNOWAIT|__WALL)` must return ECHILD. A nil return means children exist, even if none is waitable, and is rejected. This monitor must have no unrelated trusted children, no concurrent fork/spawn/wait owner, and no foreign/manual/CGO threads. Go runtime threads are allowed. These remain trusted caller invariants; the handle is not an execution API. Future target integration must serialize the one user spawn to completion before beginning drain, close management descriptors in that child, durably accept before spawn, and prohibit all new trusted spawns once drain starts. No `os.Process.Wait` may compete with the monitor's reap loop.

Unsupported platforms/architectures return ErrUnsupported without mutation. Supported scope is CGO_ENABLED=0 Go-owned threads on native Linux amd64/arm64 with seccomp and the required proc fields (Seccomp_filters was introduced in Linux 5.9). Missing required facilities return ErrUnsupported or ErrKernelUnavailable with original causes; malformed or incompatible identity/policy returns ErrUnsafeKernel. Existing error sentinels are reused.

## Inherited seccomp layer

Classic BPF generated in Go uses architecture-specific native syscall numbers. Check audit architecture before syscall number; foreign ABI returns SECCOMP_RET_KILL_PROCESS. On amd64 deny the x32 syscall bit with ENOSYS, and syscall numbers 512..547 with ENOSYS, before evaluating the native deny rules. No compat-ABI bypass. Native arm64 uses AUDIT_ARCH_AARCH64; amd64 uses AUDIT_ARCH_X86_64.

Return EACCES for unshare, setns, ptrace, process_vm_readv, process_vm_writev, and legacy clone with any of CLONE_NEWNS, CLONE_NEWCGROUP, CLONE_NEWUTS, CLONE_NEWIPC, CLONE_NEWUSER, CLONE_NEWPID, CLONE_NEWNET, CLONE_NEWTIME. Check the low 32 flag bits of clone argument zero for these two native architectures. Return ENOSYS for clone3: classic BPF cannot inspect its pointed-to flags. Ordinary clone/fork/thread/exec remains allowed by this added layer, subject to existing outer filters. This does not claim Go clone3 fallback: actual Go 1.25.6 uses legacy clone for ordinary exec and explicitly chosen clone3 paths return their error. Users requiring clone3-only features, namespace creation, compat ABI or tracing incur a deliberate compatibility restriction.

Install the fixed filter with all-Go-thread PR_SET_SECCOMP(SECCOMP_MODE_FILTER) after the monitor kernel boundary is prepared. Pointer arguments must use the existing uintptrescapes/KeepAlive pattern; do not issue single-thread setters. Before installation read every thread's Seccomp and Seccomp_filters, strictly rejecting missing/duplicate/invalid fields, mode 1, or nonuniform observations. Mode 0 requires count 0; mode 2 requires count >0. Counts must be less than 4096 before adding the layer. Afterwards every actual thread must have mode 2 and count exactly old count+1. Newly created threads must inherit it. Use bounded page128/max4096 thread traversal and at most three retries for vanished threads, as in the existing kernel contract.

Filter counts are diagnostics; they are not a cryptographic policy measurement. The guarantee depends on actual successful installation of this fixed policy and the trusted runtime/caller contract. ValidateCurrent checks actual kernel boundary, all-thread mode 2 and count at least the installed count, and cgroup state. It never reinstalls or repairs a filter. Layered filters may restrict operations further; same-action ERRNO data from this newest layer can make EACCES attributable in the supported native fixture, while higher precedence external actions still win.

## Cgroup preflight

Read `/proc/self/mountinfo` with a 1 MiB maximum, 4096-line maximum and 16 KiB line maximum. Parse its mandatory fields and separator strictly. Require at least one cgroup or cgroup2 mount and require every such mount's per-mount options to contain exact `ro` and not `rw`. Do not confuse superblock rw with a writable per-mount view. No writable alternate bind is allowed. No namespace-changing syscall is allowed after confinement. Read and retain the bounded canonical `/proc/self/cgroup` bytes (maximum4096) with validated nonempty, unique hierarchy lines. ValidateCurrent rechecks read-only mounts and exact membership. A user child must have exact same cgroup membership. External host changes or missing/ambiguous observations fail closed rather than certifying the original bound.

These checks assume trusted unchanged mount topology and that future spawn closes management/writable resource FDs. They do not authenticate the host or prevent an administrator changing cgroups. No idle polling/FD retention is introduced by these checks.

## Drain algorithm and errors

Drain requires a nonnil context with a live deadline, no more than30seconds away. Local monotonic deadlines are cleanup budgets, not authenticated UTC. Validate parameters before touching children. One method owner at a time uses nonblocking mutex acquisition; concurrent operations fail ErrKernelUnavailable rather than waiting behind an active drain. No background goroutine survives return.

1. Validate the actual sealed monitor boundary and no-new-spawn/sole-wait caller contract.
2. Reap exited children using `wait4(-1, WNOHANG|__WALL)`, retrying EINTR while checking context. Retain raw statuses for at most4096 reaped children in this handle; only exited/signaled statuses count, not stopped/continued observations. ECHILD is a candidate completion, not an inference from a root exit code.
3. If children remain, scan only the current namespace `/proc` in pages of128, with at most4096 numeric process entries per pass. No global retained PID map. For each process use an O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC proc-directory FD, openat its status with O_NOFOLLOW|O_CLOEXEC, and limit status to64KiB. Parse Pid/Tgid/PPid strictly; inspect further only when PPid is the current monitor. The held directory FD anchors signal identity. Require leader identity, all four UID/GID values equal the sealed nonzero identity, all five cap sets zero, NoNewPrivs=1, Seccomp=2, filter count >=installed count and same cgroup bytes. Do not reuse the thread parser's Threads>=1 assumption for zombies.
4. Reread that same pinned child's identity and PPid immediately before `pidfd_send_signal(procFD, SIGKILL)`. Only this actual direct child is signaled. No integer PID kill or signal-to-group fallback. The FD never retargets a recycled PID. Close each child's handles before visiting another; at most one directory page and one child status/membership are retained. Never signal PID1, self, another monitor or another monitor's child.
5. Repeat reap/scan until actual __WALL ECHILD, then revalidate actual confinement and check context again. Return a copied LocalDrainObservation only then. The loop may wait5milliseconds with a context-aware timer while children are outstanding; no timer exists for idle handles.

ENOENT/ESRCH from a pinned vanished child may cause rescan/reap, never completion by itself. Every other permission/I/O/identity/overflow/resource-limit uncertainty returns an error. On cancellation, timeout, unsafe state or operational failure, poison this handle for further success and return a zero observation; future target must keep unknown/gate closed and escalate to PID1 namespace cleanup. No detached cleanup worker, rollback, or fake End. Invalid context and concurrent-operation rejection before drain do not poison the valid owner. Repeated successful Drain must inspect actual state again; cached diagnostics alone never certify terminal. Child creation after drain began violates the caller contract and must remain a target integration review concern.

Context and bounded userspace loops do not preempt arbitrary kernel I/O, uninterruptible tasks, or an unhealthy Docker daemon. Such failure is an availability/unknown outcome, never permission to release workspace ownership. Monitor crash loses command attribution and requires a later PID1-wide cleanup; that path and remote-write settlement are outside this local component.

## Verification and delivery

Task1 implements confinement, bounded parsers, cross-platform failure and real inheritance/denial tests. Task2 implements the sole-owner local drainer and real adversarial descendant tests. Each publishes small verified commits immediately, independently reviewed against its full range. Final review covers the whole new range and named unchanged kernel/target/protocol integration seams, reusing the already-reviewed predecessor baseline with its retained limits.

Host parser/contract tests and host race are separate from CGO0 Linux native gates. Native tests use the already-local pinned image and actual PID1/root monitor exec, NNP and the existing four-cap startup fixture. No downloads, host namespaces, privileged mode or extra caps. Fixed test-only modes are permitted, never a product raw-start API. Record actual proc state, syscall errors, child statuses, command outcomes, source/binary digests and cleanup. The controller may run the existing nine-mode script with new positive tests and explicit current workspace evidence path. Exact owned resource removals and own-label inventories must succeed.

Required actual tests: ordinary Go thread/fork/exec works after installation; forbidden syscalls return this layer's EACCES and clone3 ENOSYS; new threads/user exec inherit count; cgroup writes fail; nil/wrong-role/repeat/invalid identity reject; no-child/no-waiter preflight; setsid and double fork with root exited but live grandchild; a user subreaper; CLONE_PARENT and non-SIGCHLD clone child; two concurrent monitors with same UID, draining one leaves the other's children alive; SIGCONT interference does not create terminal success while any owned child lives; old pinned proc handle returns ESRCH after exit rather than signaling a replacement; context/error/limit failures return no observation. A non-SIGCHLD clone helper must be a separate trusted test binary/process with raw-syscall-safe code, not a Go goroutine continuing after raw fork.

If an actual native counterexample changes an assumption, amend the spec and ledger before implementation changes, retaining the original failed evidence. Do not claim Linux race, full ABI/kernel/LSM matrix, power-loss, transport, business End, remote-write drainage, fleet throughput or production quota delivery from these local tests.

## Sources and explicit costs

The architecture/clone3/x32/error-precedence rules follow [Linux seccomp](https://man7.org/linux/man-pages/man2/seccomp.2.html) and [kernel seccomp filtering](https://kernel.org/doc/html/latest/userspace-api/seccomp_filter.html). Required field availability follows [proc status](https://www.man7.org/linux/man-pages/man5/proc_pid_status.5.html). Direct-child reaping includes clone children under [wait __WALL](https://man7.org/linux/man-pages/man2/waitpid.2.html). Pinned proc-directory signaling follows [pidfd_send_signal](https://man7.org/linux/man-pages/man2/pidfd_send_signal.2.html); nearest-subreaper reasoning follows [PR_SET_CHILD_SUBREAPER](https://man7.org/linux/man-pages/man2/PR_SET_CHILD_SUBREAPER.2const.html). The repeated direct-child fixed point is our inference under the stated no-new-spawn/namespace/tracing/caller constraints and requires actual tests.

Costs: extra active-command monitor process; inherited kernel filter storage and denied user features; Linux5.9/proc compatibility floor; bounded4096 process/reap limits and30second budget can force unknown; poison-on-failure requires later namespace escalation. They do not expand the original sandbox quotas. Actual idle/active RSS, startup and minimum-quota measurements remain mandatory during image/production integration. Logical large-N gains still require shared watches, scheduler, workers, collector, GC and final Redis removal.
