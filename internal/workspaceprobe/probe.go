// Package workspaceprobe implements the fixed, unprivileged workspace
// propagation and quiescence probe used inside FUSE-capable sandboxes.
package workspaceprobe

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	defaultWorkspace = "/workspace"
	requiredUID      = 1000
	requiredGID      = 1000
	probePayloadSize = 32
)

var (
	ErrWrongUser                 = errors.New("workspace probe must run as uid/gid 1000")
	ErrWritableWorkspaceFD       = errors.New("a process holds a writable workspace descriptor")
	ErrInvalidToken              = errors.New("invalid or consumed quiesce token")
	ErrIOTimeout                 = errors.New("workspace probe I/O timed out")
	ErrIOCompensationUnconfirmed = errors.New("workspace probe cleanup is unconfirmed")
)

type processIdentity struct {
	PID       int
	UID       int
	StartTime uint64
	State     byte
}

type descriptor struct {
	Target string
	Flags  int
}

type processTable interface {
	listSameUID(uid, selfPID int) ([]processIdentity, error)
	signal(pid int, signal syscall.Signal) error
	identity(pid int) (processIdentity, error)
	descriptors(pid int) ([]descriptor, error)
}

type brokerState struct {
	RuntimeUID string
	Generation int64
	Processes  []processIdentity
}

type resumeRequest struct {
	RuntimeUID string
	Generation int64
	Token      string
}

type brokerClient interface {
	start(state brokerState) (string, error)
	resume(request resumeRequest) error
}

type brokerProcessProtector interface {
	protectedProcess() (*processIdentity, error)
}

type mountVerifier interface {
	requireS3FSMount(path string) error
}

type workspaceIOBoundary interface {
	run(runtimeUID string, generation int64) error
}

type config struct {
	workspace  string
	uid        int
	gid        int
	selfPID    int
	euid       func() int
	egid       func() int
	processes  processTable
	broker     brokerClient
	mounts     mountVerifier
	random     io.Reader
	chdir      func(string) error
	ioBoundary workspaceIOBoundary
}

type Probe struct {
	workspace  string
	uid        int
	gid        int
	selfPID    int
	euid       func() int
	egid       func() int
	processes  processTable
	broker     brokerClient
	mounts     mountVerifier
	random     io.Reader
	chdir      func(string) error
	ioBoundary workspaceIOBoundary
}

// New returns the production probe. Its workspace and execution identity are
// compile-time constants so callers cannot turn the privileged runtime control
// path into an arbitrary file or process operation.
func New() *Probe {
	return newProbe(config{})
}

func newProbe(cfg config) *Probe {
	if cfg.workspace == "" {
		cfg.workspace = defaultWorkspace
	}
	if cfg.uid == 0 {
		cfg.uid = requiredUID
	}
	if cfg.gid == 0 {
		cfg.gid = requiredGID
	}
	if cfg.selfPID == 0 {
		cfg.selfPID = os.Getpid()
	}
	if cfg.euid == nil {
		cfg.euid = os.Geteuid
	}
	if cfg.egid == nil {
		cfg.egid = os.Getegid
	}
	if cfg.processes == nil {
		cfg.processes = procFS{root: "/proc"}
	}
	if cfg.random == nil {
		cfg.random = rand.Reader
	}
	if cfg.chdir == nil {
		cfg.chdir = os.Chdir
	}
	if cfg.broker == nil {
		cfg.broker = newHybridBroker(cfg.processes, cfg.random)
	}
	if cfg.mounts == nil {
		cfg.mounts = procMountInfo{path: "/proc/self/mountinfo"}
	}
	probe := &Probe{
		workspace:  cfg.workspace,
		uid:        cfg.uid,
		gid:        cfg.gid,
		selfPID:    cfg.selfPID,
		euid:       cfg.euid,
		egid:       cfg.egid,
		processes:  cfg.processes,
		broker:     cfg.broker,
		mounts:     cfg.mounts,
		random:     cfg.random,
		chdir:      cfg.chdir,
		ioBoundary: cfg.ioBoundary,
	}
	if probe.ioBoundary == nil {
		probe.ioBoundary = newSubprocessIOBoundary()
	}
	return probe
}

func (p *Probe) SelfCheck() error {
	if err := p.checkUser(); err != nil {
		return err
	}
	info, err := os.Stat(p.workspace)
	if err != nil {
		return fmt.Errorf("workspace is unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace is not a directory")
	}
	return nil
}

func (p *Probe) WriteReadDelete(runtimeUID string, generation int64) (fuseprotocol.ProbeStatus, error) {
	if err := p.validateRequest(runtimeUID, generation); err != nil {
		return fuseprotocol.ProbeStatus{}, err
	}
	if err := p.ioBoundary.run(runtimeUID, generation); err != nil {
		return fuseprotocol.ProbeStatus{}, err
	}
	return successStatus(runtimeUID, generation, ""), nil
}

func (p *Probe) writeReadDeleteDirect(runtimeUID string, generation int64, name string) error {
	if err := p.validateRequest(runtimeUID, generation); err != nil {
		return err
	}
	if !validDerivedProbeName(runtimeUID, generation, name) {
		return fmt.Errorf("invalid workspace probe nonce")
	}
	if err := p.mounts.requireS3FSMount(p.workspace); err != nil {
		return fmt.Errorf("workspace mount is not ready: %w", err)
	}
	payload := make([]byte, probePayloadSize)
	if _, err := io.ReadFull(p.random, payload); err != nil {
		return fmt.Errorf("generate probe payload: %w", err)
	}
	defer wipe(payload)

	path := filepath.Join(p.workspace, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create workspace probe: %w", err)
	}
	created := true
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("write workspace probe: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync workspace probe: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close written workspace probe: %w", err)
	}
	file = nil
	file, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("reopen workspace probe: %w", err)
	}
	readBack, err := io.ReadAll(io.LimitReader(file, probePayloadSize+1))
	if err != nil {
		return fmt.Errorf("read workspace probe: %w", err)
	}
	if !bytes.Equal(payload, readBack) {
		return fmt.Errorf("workspace probe content mismatch")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close workspace probe: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("delete workspace probe: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("deleted workspace probe still exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("verify deleted workspace probe: %w", err)
	}
	created = false
	if err := p.mounts.requireS3FSMount(p.workspace); err != nil {
		return fmt.Errorf("workspace mount changed during probe: %w", err)
	}
	return nil
}

func (p *Probe) cleanupProbeDirect(runtimeUID string, generation int64, name string) error {
	if err := p.validateRequest(runtimeUID, generation); err != nil {
		return err
	}
	if !validDerivedProbeName(runtimeUID, generation, name) {
		return fmt.Errorf("invalid workspace probe nonce")
	}
	if err := p.mounts.requireS3FSMount(p.workspace); err != nil {
		return fmt.Errorf("workspace mount is not ready for cleanup: %w", err)
	}
	path := filepath.Join(p.workspace, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete workspace probe compensation: %w", err)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("workspace probe compensation still exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("verify workspace probe compensation: %w", err)
	}
	if err := p.mounts.requireS3FSMount(p.workspace); err != nil {
		return fmt.Errorf("workspace mount changed during cleanup: %w", err)
	}
	return nil
}

func (p *Probe) Quiesce(runtimeUID string, generation int64) (fuseprotocol.ProbeStatus, error) {
	if err := p.validateRequest(runtimeUID, generation); err != nil {
		return fuseprotocol.ProbeStatus{}, err
	}
	if err := p.chdir("/"); err != nil {
		return fuseprotocol.ProbeStatus{}, fmt.Errorf("leave workspace working directory: %w", err)
	}
	protected := map[int]processIdentity{}
	if protector, ok := p.broker.(brokerProcessProtector); ok {
		process, err := protector.protectedProcess()
		if err != nil {
			return fuseprotocol.ProbeStatus{}, fmt.Errorf("verify workspace probe pid 1: %w", err)
		}
		if process != nil {
			protected[process.PID] = *process
		}
	}
	stopped, err := p.stopToFixedPoint(protected)
	if err != nil {
		return fuseprotocol.ProbeStatus{}, err
	}
	if err := p.verifyNoWritableWorkspaceFD(stopped); err != nil {
		return fuseprotocol.ProbeStatus{}, err
	}
	token, err := p.broker.start(brokerState{RuntimeUID: runtimeUID, Generation: generation, Processes: stopped})
	if err != nil {
		return fuseprotocol.ProbeStatus{}, fmt.Errorf("start quiesce broker: %w", err)
	}
	if token == "" || len(token) > 4096 {
		return fuseprotocol.ProbeStatus{}, fmt.Errorf("quiesce broker returned an invalid token")
	}
	return successStatus(runtimeUID, generation, token), nil
}

func (p *Probe) Resume(request resumeRequest) error {
	if err := p.validateRequest(request.RuntimeUID, request.Generation); err != nil {
		return err
	}
	if request.Token == "" || len(request.Token) > 4096 {
		return ErrInvalidToken
	}
	if err := p.broker.resume(request); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	return nil
}

func (p *Probe) validateRequest(runtimeUID string, generation int64) error {
	if err := p.checkUser(); err != nil {
		return err
	}
	if !fuseprotocol.ValidIdentity(runtimeUID) || generation <= 0 {
		return fmt.Errorf("invalid runtime identity or generation")
	}
	return nil
}

func (p *Probe) checkUser() error {
	if p.euid() != p.uid || p.egid() != p.gid {
		return ErrWrongUser
	}
	return nil
}

func (p *Probe) stopToFixedPoint(protected map[int]processIdentity) ([]processIdentity, error) {
	const maxScans = 128
	stopped := make(map[int]processIdentity)
	for scan := 0; scan < maxScans; scan++ {
		processes, err := p.processes.listSameUID(p.uid, p.selfPID)
		if err != nil {
			return mapProcesses(stopped), fmt.Errorf("enumerate sandbox processes: %w", err)
		}
		added := false
		seen := make(map[int]struct{}, len(processes))
		for _, process := range processes {
			if expected, ok := protected[process.PID]; ok {
				if expected.UID != process.UID || expected.StartTime != process.StartTime {
					return mapProcesses(stopped), fmt.Errorf("protected process identity changed")
				}
				continue
			}
			seen[process.PID] = struct{}{}
			if previous, ok := stopped[process.PID]; ok {
				if previous.StartTime != process.StartTime || previous.UID != process.UID || !isStopped(process.State) {
					return mapProcesses(stopped), fmt.Errorf("stopped process identity changed")
				}
				continue
			}
			if err := p.processes.signal(process.PID, syscall.SIGSTOP); err != nil {
				return mapProcesses(stopped), fmt.Errorf("stop process %d: %w", process.PID, err)
			}
			current, err := p.waitUntilStopped(process)
			if err != nil {
				return mapProcesses(stopped), fmt.Errorf("verify stopped process %d", process.PID)
			}
			stopped[process.PID] = current
			added = true
		}
		for pid := range stopped {
			if _, ok := seen[pid]; !ok {
				return mapProcesses(stopped), fmt.Errorf("stopped process %d disappeared", pid)
			}
		}
		if !added {
			return mapProcesses(stopped), nil
		}
	}
	return mapProcesses(stopped), fmt.Errorf("process set did not reach a fixed point")
}

func (p *Probe) waitUntilStopped(expected processIdentity) (processIdentity, error) {
	const attempts = 50
	for attempt := 0; attempt < attempts; attempt++ {
		current, err := p.processes.identity(expected.PID)
		if err != nil {
			return processIdentity{}, err
		}
		if current.UID != p.uid || current.StartTime != expected.StartTime {
			return processIdentity{}, fmt.Errorf("process identity changed")
		}
		if isStopped(current.State) {
			return current, nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return processIdentity{}, fmt.Errorf("process did not stop")
}

func (p *Probe) verifyNoWritableWorkspaceFD(processes []processIdentity) error {
	for _, process := range processes {
		current, err := p.processes.identity(process.PID)
		if err != nil || current.UID != process.UID || current.StartTime != process.StartTime || !isStopped(current.State) {
			return fmt.Errorf("process %d is no longer provably stopped", process.PID)
		}
		descriptors, err := p.processes.descriptors(process.PID)
		if err != nil {
			return fmt.Errorf("enumerate descriptors for process %d: %w", process.PID, err)
		}
		for _, fd := range descriptors {
			mode := fd.Flags & syscall.O_ACCMODE
			if mode != syscall.O_RDONLY && targetInWorkspace(fd.Target, p.workspace) {
				return fmt.Errorf("%w: pid %d", ErrWritableWorkspaceFD, process.PID)
			}
		}
	}
	return nil
}

func successStatus(runtimeUID string, generation int64, token string) fuseprotocol.ProbeStatus {
	return fuseprotocol.ProbeStatus{Version: fuseprotocol.Version, RuntimeUID: runtimeUID, Generation: generation, OK: true, Token: token}
}

func validDerivedProbeName(runtimeUID string, generation int64, name string) bool {
	expected, err := fuseprotocol.DeriveProbeObjectName(runtimeUID, generation)
	return err == nil && name == expected
}

func targetInWorkspace(target, workspace string) bool {
	target = filepath.Clean(trimDeletedSuffix(target))
	workspace = filepath.Clean(workspace)
	return target == workspace || len(target) > len(workspace) && target[:len(workspace)] == workspace && target[len(workspace)] == filepath.Separator
}

func trimDeletedSuffix(target string) string {
	const suffix = " (deleted)"
	if len(target) >= len(suffix) && target[len(target)-len(suffix):] == suffix {
		return target[:len(target)-len(suffix)]
	}
	return target
}

func mapProcesses(processes map[int]processIdentity) []processIdentity {
	result := make([]processIdentity, 0, len(processes))
	for _, process := range processes {
		result = append(result, process)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result
}

func isStopped(state byte) bool { return state == 'T' || state == 't' }

func wipe(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
