package mounter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// NativeConfig supplies trusted local storage paths to the production helper.
// It cannot supply a process identity, runner, profile registry or kernel check.
// Directories may be absent while the helper is locked; Bootstrap prepares them.
type NativeConfig struct {
	RunDir         string
	CredentialRoot string
	CacheRoot      string
	MountPath      string
}

type nativeFileIdentity struct {
	device uint64
	inode  uint64
}

type nativeIdentity struct {
	pid int

	// startTime is /proc/<pid>/stat field 22, in kernel clock ticks since boot.
	startTime uint64
	bootID    string

	proc           nativeFileIdentity
	pidNamespace   nativeFileIdentity
	mountNamespace nativeFileIdentity
	executable     nativeFileIdentity
}

// nativeOwner retains the original kernel objects, rather than treating saved
// identity strings as authority. supervisor is bound once before publication;
// it prevents a copied Supervisor from borrowing or closing this ownership.
// After publication Supervisor.mu owns all mutable fields and file lifetimes.
// Revocation seals admission but retains handles for the original cleanup.
type nativeOwner struct {
	self       *nativeOwner
	supervisor *Supervisor
	identity   nativeIdentity
	revoked    bool
	closed     bool
	closeErr   error

	proc           *os.File
	pidNamespace   *os.File
	mountNamespace *os.File
	executable     *os.File
	pidfd          *os.File
}

var errNativeClosePending = errors.New("native helper close pending: original process has not joined")

// NewNativeSupervisor captures the actual local PID1 incarnation and installs
// only the bundled profiles, real kernel/filesystem checks and CommandRunner.
// It proves local process continuity, not helper-to-Pod binding or confinement.
// The caller must join its services, attempt Shutdown, then Close on every exit.
func NewNativeSupervisor(config NativeConfig) (*Supervisor, error) {
	paths := []struct {
		name  string
		value string
	}{
		{"run directory", config.RunDir},
		{"credential root", config.CredentialRoot},
		{"cache root", config.CacheRoot},
		{"mount path", config.MountPath},
	}
	for _, path := range paths {
		if !filepath.IsAbs(path.value) || filepath.Clean(path.value) != path.value {
			return nil, fmt.Errorf("native helper %s must be a clean absolute path", path.name)
		}
	}

	owner, err := captureNativeOwner()
	if err != nil {
		return nil, err
	}

	supervisor := NewSupervisor(Config{
		RunDir:         config.RunDir,
		CredentialRoot: config.CredentialRoot,
		CacheRoot:      config.CacheRoot,
		MountPath:      config.MountPath,
		Profiles:       BundledProfiles(),
		CheckFuse:      CheckFuseDevice,
		PrepareAnchor:  PrepareWorkspaceAnchor,
		CheckAnchor:    CheckWorkspaceAnchor,
		MountInfo: func() (Mount, error) {
			return ReadEffectiveMount(config.MountPath)
		},
	}, CommandRunner{})
	owner.supervisor = supervisor
	supervisor.nativeOwner = owner
	return supervisor, nil
}

// checkNativeBinding runs before taking mu: a value copy may contain a copied
// locked mutex as well as shared files. Reject it without touching either the
// copied state or the original owner's revocation and cleanup state.
func (s *Supervisor) checkNativeBinding() error {
	if s.nativeOwner == nil {
		return nil
	}
	if s.nativeOwner.self != s.nativeOwner || s.nativeOwner.supervisor != s {
		return fmt.Errorf("native helper owner belongs to another supervisor instance")
	}
	return nil
}

func (s *Supervisor) validateNativeLocked() error {
	if err := s.checkNativeBinding(); err != nil {
		return err
	}
	if s.nativeOwner == nil {
		return nil
	}
	return s.nativeOwner.validateCurrent()
}

// lockNativeThread keeps the final identity check and its subsequent local
// publication/Start on the inspected thread. It neither scans nor hardens other
// threads. The model constructor retains its original scheduling behavior.
func (s *Supervisor) lockNativeThread() func() {
	if s.nativeOwner == nil {
		return func() {}
	}
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}

// Close permanently seals native admission and releases kernel handles only
// after the original child has actually joined. A pending result retains all
// handles and permits the original Shutdown to be retried. Close does not prove
// unmount, remote durability or business safe release, and never replaces
// Shutdown. Repeated Close is safe after completion; copied instances are refused.
func (s *Supervisor) Close() error {
	if err := s.checkNativeBinding(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nativeOwner == nil {
		return nil
	}

	s.nativeOwner.revoked = true
	if s.process != nil {
		if s.processDone == nil {
			return errNativeClosePending
		}
		select {
		case <-s.processDone:
		default:
			return errNativeClosePending
		}
	}
	return s.nativeOwner.close()
}

// close also handles partial construction. Mark closed before releasing files,
// then visit every original handle in reverse acquisition order even on error.
// Clear each reference so a repeated close cannot touch a reused descriptor;
// retain the first complete close result so retries cannot hide cleanup errors.
func (owner *nativeOwner) close() error {
	if owner == nil {
		return nil
	}
	if owner.self != owner {
		return fmt.Errorf("cannot close a copied native helper owner")
	}
	if owner.closed {
		return owner.closeErr
	}
	owner.revoked, owner.closed = true, true
	for _, slot := range []**os.File{
		&owner.pidfd,
		&owner.executable,
		&owner.mountNamespace,
		&owner.pidNamespace,
		&owner.proc,
	} {
		if *slot != nil {
			owner.closeErr = errors.Join(owner.closeErr, (*slot).Close())
			*slot = nil
		}
	}
	return owner.closeErr
}
