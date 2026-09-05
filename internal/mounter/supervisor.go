// Package mounter implements the single-use trusted s3fs supervisor.
package mounter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const mountGenerationFile = "mount-generation"

type State string

const (
	StateLocked          State = "locked"
	StatePrepared        State = "prepared"
	StateMounting        State = "mounting"
	StateReady           State = "ready"
	StateUnhealthy       State = "unhealthy"
	StateRestartDetected State = "restart-detected"
	StateStopped         State = "stopped"
)

var (
	ErrAuthorizationConsumed = errors.New("workspace authorization already consumed")
	ErrBootstrapConsumed     = errors.New("workspace bootstrap already consumed")
	ErrFlushUnsupported      = errors.New("profile has no verified non-destructive flush strategy")
)

type Process interface {
	Wait() error
	Signal(os.Signal) error
}

type Runner interface {
	Start(ctx context.Context, argv, environment []string) (Process, error)
	Run(ctx context.Context, argv []string) error
}

type Config struct {
	RunDir            string
	CredentialRoot    string
	CacheRoot         string
	MountPath         string
	Profiles          ProfileRegistry
	CheckFuse         func() error
	PrepareAnchor     func(path string) error
	CheckAnchor       func(path string) error
	MountInfo         func() (Mount, error)
	MountPollInterval time.Duration
}

type Supervisor struct {
	mu            sync.Mutex
	config        Config
	runner        Runner
	state         State
	bootstrap     fuseprotocol.BootstrapConfig
	profile       Profile
	auth          fuseprotocol.AuthorizeRequest
	process       Process
	processDone   chan struct{}
	bootstrapped  bool
	consumed      bool
	stopping      bool
	shutdownTried bool
	mountID       uint64
	mountDeadline time.Time
}

func NewSupervisor(config Config, runner Runner) *Supervisor {
	if config.Profiles == nil {
		config.Profiles = CompiledProfiles()
	}
	s := &Supervisor{config: config, runner: runner, state: StateLocked}
	if _, err := os.Lstat(filepath.Join(config.RunDir, mountGenerationFile)); err == nil {
		s.state, s.consumed = StateRestartDetected, true
	} else if !os.IsNotExist(err) {
		s.state, s.consumed = StateUnhealthy, true
	}
	return s
}

func (s *Supervisor) State() State { s.mu.Lock(); defer s.mu.Unlock(); return s.state }

func (s *Supervisor) RecoverRestarted(expected fuseprotocol.BootstrapConfig, expectedRuntimeUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateRestartDetected || !s.consumed || s.bootstrapped {
		return fmt.Errorf("supervisor is not awaiting restart recovery")
	}
	bootstrapPath := filepath.Join(s.config.RunDir, "bootstrap.json")
	markerPath := filepath.Join(s.config.RunDir, mountGenerationFile)
	if err := secureDirectory(s.config.RunDir, 0o700); err != nil {
		return fmt.Errorf("restart run directory is not trusted")
	}
	if err := securePublishedFile(bootstrapPath); err != nil {
		return fmt.Errorf("restart bootstrap is not trusted")
	}
	if err := securePublishedFile(markerPath); err != nil {
		return fmt.Errorf("restart marker is not trusted")
	}
	bootstrapRaw, err := readBoundedFile(bootstrapPath)
	if err != nil {
		return err
	}
	var persisted fuseprotocol.BootstrapConfig
	if err := fuseprotocol.Decode(bootstrapRaw, &persisted); err != nil {
		return fmt.Errorf("restart bootstrap schema is invalid")
	}
	if expected.Version != 0 {
		if expectedRuntimeUID == "" || (expected.RuntimeUID != "" && expected.RuntimeUID != expectedRuntimeUID) {
			return fmt.Errorf("restart expected runtime identity is invalid")
		}
		expected.RuntimeUID = expectedRuntimeUID
		if expected != persisted {
			return fmt.Errorf("restart bootstrap identity changed")
		}
	} else if expectedRuntimeUID != "" && persisted.RuntimeUID != expectedRuntimeUID {
		return fmt.Errorf("restart runtime identity changed")
	}
	profile, ok := s.config.Profiles.Lookup(persisted.Profile)
	if persisted.Version != fuseprotocol.Version || !fuseprotocol.ValidIdentity(persisted.RuntimeUID) || !validHexDigest(persisted.PoolKey) || !ok || profile.Provider != persisted.Provider || profile.Options == nil {
		return fmt.Errorf("restart bootstrap identity is invalid")
	}
	markerRaw, err := readBoundedFile(markerPath)
	if err != nil {
		return err
	}
	var authorization fuseprotocol.AuthorizeRequest
	if err := fuseprotocol.DecodeExact(markerRaw, &authorization); err != nil || authorization.Version != fuseprotocol.Version || authorization.RuntimeUID != persisted.RuntimeUID || authorization.PoolKey != persisted.PoolKey || !validHexDigest(authorization.WorkspaceHash) || !fuseprotocol.ValidCanonicalPrefix(authorization.Prefix) || authorization.LeaseGeneration <= 0 || authorization.MountAttempt != 1 {
		return fmt.Errorf("restart mount marker identity is invalid")
	}
	s.bootstrap, s.profile, s.auth, s.bootstrapped = persisted, profile, authorization, true
	return nil
}

func (s *Supervisor) Bootstrap(_ context.Context, bootstrap fuseprotocol.BootstrapConfig, expectedRuntimeUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bootstrapped || s.state != StateLocked {
		return ErrBootstrapConsumed
	}
	if err := s.validateBootstrap(bootstrap, expectedRuntimeUID); err != nil {
		return err
	}
	bootstrap.RuntimeUID = expectedRuntimeUID
	access, err := readCredential(bootstrap.AccessKeyFile, s.config.CredentialRoot, true)
	if err != nil {
		return err
	}
	defer wipe(access)
	secret, err := readCredential(bootstrap.SecretKeyFile, s.config.CredentialRoot, false)
	if err != nil {
		return err
	}
	defer wipe(secret)
	passwd := make([]byte, 0, len(access)+len(secret)+2)
	passwd = append(passwd, access...)
	passwd = append(passwd, ':')
	passwd = append(passwd, secret...)
	passwd = append(passwd, '\n')
	defer wipe(passwd)
	if err := atomicPublish(bootstrap.PasswdFile, passwd, 0o600); err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("publish s3fs credential file: %w", err)
	}
	sanitized, err := json.Marshal(bootstrap)
	if err != nil {
		return fmt.Errorf("marshal sanitized bootstrap: %w", err)
	}
	if err := atomicPublish(filepath.Join(s.config.RunDir, "bootstrap.json"), sanitized, 0o600); err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("publish sanitized bootstrap: %w", err)
	}
	s.bootstrap, s.profile, s.bootstrapped, s.state = bootstrap, mustProfile(s.config.Profiles, bootstrap.Profile), true, StatePrepared
	return nil
}

func (s *Supervisor) validateBootstrap(b fuseprotocol.BootstrapConfig, expectedUID string) error {
	if s.runner == nil || s.config.CheckFuse == nil || s.config.CheckAnchor == nil || s.config.MountInfo == nil {
		return fmt.Errorf("supervisor trusted checks are incomplete")
	}
	if b.Version != fuseprotocol.Version || !fuseprotocol.ValidIdentity(expectedUID) || (b.RuntimeUID != "" && b.RuntimeUID != expectedUID) {
		return fmt.Errorf("bootstrap runtime identity is invalid")
	}
	if !boundedBootstrapStrings(b) {
		return fmt.Errorf("bootstrap field size or encoding is invalid")
	}
	b.RuntimeUID = expectedUID
	profile, ok := s.config.Profiles.Lookup(b.Profile)
	if !ok || profile.Provider != b.Provider || profile.Options == nil {
		return fmt.Errorf("bootstrap profile is not compiled into this image")
	}
	if _, err := profile.Options(b); err != nil {
		return fmt.Errorf("bootstrap profile parameters are invalid")
	}
	if !validHexDigest(b.PoolKey) || !validBucket(b.Bucket) {
		return fmt.Errorf("bootstrap storage identity is invalid")
	}
	endpoint, err := url.Parse(b.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return fmt.Errorf("bootstrap endpoint is invalid")
	}
	if !boundedTimeout(b.MountTimeoutSeconds) || !boundedTimeout(b.FlushTimeoutSeconds) || !boundedTimeout(b.UnmountTimeoutSeconds) {
		return fmt.Errorf("bootstrap timeouts are invalid")
	}
	if b.PasswdFile != filepath.Join(s.config.RunDir, "passwd-s3fs") || filepath.Clean(b.PasswdFile) != b.PasswdFile || b.MountPath != s.config.MountPath || !withinRootOrSelf(b.CacheDir, s.config.CacheRoot) || !withinRoot(b.AccessKeyFile, s.config.CredentialRoot) || !withinRoot(b.SecretKeyFile, s.config.CredentialRoot) || (b.CAFile != "" && !withinRoot(b.CAFile, s.config.CredentialRoot)) {
		return fmt.Errorf("bootstrap path is outside its trusted root")
	}
	if b.AccessKeyFile == b.SecretKeyFile || b.AccessKeyFile == b.PasswdFile || b.SecretKeyFile == b.PasswdFile {
		return fmt.Errorf("bootstrap input and output paths overlap")
	}
	resolvedAccess, accessErr := filepath.EvalSymlinks(b.AccessKeyFile)
	resolvedSecret, secretErr := filepath.EvalSymlinks(b.SecretKeyFile)
	if accessErr != nil || secretErr != nil || resolvedAccess == resolvedSecret {
		return fmt.Errorf("bootstrap credential sources are invalid or overlap")
	}
	if err := prepareSecureDirectory(s.config.RunDir, 0o700); err != nil {
		return err
	}
	if err := secureResolvedDirectory(b.CacheDir, s.config.CacheRoot); err != nil {
		return fmt.Errorf("validate cache directory: %w", err)
	}
	if err := prepareCacheTemp(filepath.Join(b.CacheDir, "tmp"), s.config.CacheRoot); err != nil {
		return fmt.Errorf("validate cache temporary directory: %w", err)
	}
	for _, path := range []string{b.AccessKeyFile, b.SecretKeyFile} {
		if err := secureResolvedCredential(path, s.config.CredentialRoot); err != nil {
			return fmt.Errorf("validate credential source: %w", err)
		}
	}
	if b.CAFile != "" {
		if err := secureResolvedFile(b.CAFile, s.config.CredentialRoot); err != nil {
			return fmt.Errorf("validate CA source: %w", err)
		}
	}
	if s.config.CheckFuse != nil {
		if err := s.config.CheckFuse(); err != nil {
			return fmt.Errorf("check fuse device: %w", err)
		}
	}
	prepareAnchor := s.config.PrepareAnchor
	if prepareAnchor == nil {
		prepareAnchor = s.config.CheckAnchor
	}
	if prepareAnchor != nil {
		if err := prepareAnchor(b.MountPath); err != nil {
			return fmt.Errorf("check workspace anchor: %w", err)
		}
	}
	return nil
}

func (s *Supervisor) Authorize(ctx context.Context, auth fuseprotocol.AuthorizeRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.consumed || s.state == StateRestartDetected || s.state == StateUnhealthy {
		return ErrAuthorizationConsumed
	}
	if !s.bootstrapped || s.state != StatePrepared {
		return fmt.Errorf("supervisor is not prepared")
	}
	if err := s.checkPreparedLocked(); err != nil {
		return fmt.Errorf("prepared state revalidation failed: %w", err)
	}
	if auth.Version != fuseprotocol.Version || auth.RuntimeUID != effectiveRuntimeUID(s.bootstrap) || auth.PoolKey != s.bootstrap.PoolKey || !validHexDigest(auth.WorkspaceHash) || !fuseprotocol.ValidCanonicalPrefix(auth.Prefix) || auth.LeaseGeneration <= 0 || auth.MountAttempt != 1 {
		return fmt.Errorf("workspace authorization is invalid")
	}
	s.consumed, s.auth, s.state = true, auth, StateMounting
	s.mountDeadline = time.Now().Add(time.Duration(s.bootstrap.MountTimeoutSeconds) * time.Second)
	marker, _ := json.Marshal(auth)
	if err := createExclusiveSynced(filepath.Join(s.config.RunDir, mountGenerationFile), marker, 0o600); err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("consume mount generation: %w", err)
	}
	options, err := s.profile.Options(s.bootstrap)
	if err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("build fixed s3fs profile: %w", err)
	}
	argv := []string{"/usr/bin/s3fs", s.bootstrap.Bucket + ":/" + strings.TrimSuffix(auth.Prefix, "/"), s.bootstrap.MountPath, "-f", "-o", "allow_other", "-o", "uid=1000", "-o", "gid=1000", "-o", "umask=0022", "-o", "mp_umask=0022", "-o", "passwd_file=" + s.bootstrap.PasswdFile, "-o", "tmpdir=" + filepath.Join(s.bootstrap.CacheDir, "tmp")}
	argv = append(argv, options...)
	var environment []string
	if s.bootstrap.CAFile != "" {
		environment = []string{"CURL_CA_BUNDLE=" + s.bootstrap.CAFile}
	}
	process, err := s.runner.Start(context.WithoutCancel(ctx), argv, environment)
	if err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("start foreground s3fs: %w", err)
	}
	s.process = process
	s.processDone = make(chan struct{})
	go s.reap(process, s.processDone)
	return nil
}

func (s *Supervisor) reap(process Process, done chan struct{}) {
	_ = process.Wait()
	close(done)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.process == process && !s.stopping {
		s.state = StateUnhealthy
	}
}

func (s *Supervisor) PreparedStatus() (fuseprotocol.MounterStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statusLocked()
	if s.state != StatePrepared || s.consumed || s.process != nil {
		return status, fmt.Errorf("supervisor is not pristine prepared")
	}
	if err := s.checkPreparedLocked(); err != nil {
		return status, err
	}
	return status, nil
}

func (s *Supervisor) checkPreparedLocked() error {
	if err := secureDirectory(s.config.RunDir, 0o700); err != nil {
		return fmt.Errorf("mounter run directory is no longer trusted")
	}
	if err := s.config.CheckFuse(); err != nil {
		return fmt.Errorf("FUSE device check failed")
	}
	if err := s.config.CheckAnchor(s.bootstrap.MountPath); err != nil {
		return fmt.Errorf("workspace anchor check failed")
	}
	for _, path := range []string{s.bootstrap.AccessKeyFile, s.bootstrap.SecretKeyFile} {
		if err := secureResolvedCredential(path, s.config.CredentialRoot); err != nil {
			return fmt.Errorf("credential source check failed")
		}
	}
	if s.bootstrap.CAFile != "" {
		if err := secureResolvedFile(s.bootstrap.CAFile, s.config.CredentialRoot); err != nil {
			return fmt.Errorf("CA source check failed")
		}
	}
	for _, path := range []string{s.bootstrap.PasswdFile, filepath.Join(s.config.RunDir, "bootstrap.json")} {
		if err := securePublishedFile(path); err != nil {
			return fmt.Errorf("persisted bootstrap artifact check failed")
		}
	}
	if _, err := os.Lstat(filepath.Join(s.config.RunDir, mountGenerationFile)); !os.IsNotExist(err) {
		return fmt.Errorf("mount generation is present")
	}
	if mount, err := s.config.MountInfo(); err != nil || strings.HasPrefix(mount.FilesystemType, "fuse") {
		return fmt.Errorf("workspace already has an effective FUSE mount")
	}
	return nil
}

func (s *Supervisor) ReadyStatus(ctx context.Context) (fuseprotocol.MounterStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.statusLocked()
	if err := ctx.Err(); err != nil {
		return status, err
	}
	if (s.state != StateMounting && s.state != StateReady) || s.process == nil || s.config.MountInfo == nil {
		return status, fmt.Errorf("supervisor is not mounted")
	}
	var mount Mount
	wasMounting := s.state == StateMounting
	if s.state == StateReady {
		if err := s.verifyActiveMountLocked(s.mountID); err != nil {
			s.state = StateUnhealthy
			return s.statusLocked(), err
		}
		mount = Mount{ID: s.mountID}
	} else {
		var err error
		mount, err = s.waitForMountLocked(ctx)
		if err != nil {
			if ctx.Err() == nil || !time.Now().Before(s.mountDeadline) {
				s.state = StateUnhealthy
			}
			return s.statusLocked(), err
		}
	}
	var readCtx context.Context
	var cancel context.CancelFunc
	if wasMounting {
		readCtx, cancel = context.WithDeadline(ctx, s.mountDeadline)
	} else {
		readCtx, cancel = context.WithTimeout(ctx, time.Duration(s.bootstrap.MountTimeoutSeconds)*time.Second)
	}
	defer cancel()
	if err := s.runner.Run(readCtx, []string{"/bin/ls", "-U", "--", s.bootstrap.MountPath}); err != nil {
		if ctx.Err() == nil {
			s.state = StateUnhealthy
		}
		return s.statusLocked(), fmt.Errorf("bounded workspace read failed: %w", err)
	}
	if err := s.verifyActiveMountLocked(mount.ID); err != nil {
		s.state = StateUnhealthy
		return s.statusLocked(), err
	}
	s.mountID = mount.ID
	s.state = StateReady
	return s.statusLocked(), nil
}

func (s *Supervisor) waitForMountLocked(ctx context.Context) (Mount, error) {
	remaining := time.Until(s.mountDeadline)
	if s.mountDeadline.IsZero() || remaining <= 0 {
		return Mount{}, fmt.Errorf("workspace mount readiness timed out")
	}
	deadline := time.NewTimer(remaining)
	defer deadline.Stop()
	interval := s.config.MountPollInterval
	if interval <= 0 || interval > time.Second {
		interval = 50 * time.Millisecond
	}
	for {
		select {
		case <-s.processDone:
			return Mount{}, fmt.Errorf("s3fs process exited before mount became ready")
		default:
		}
		mount, err := s.config.MountInfo()
		if err == nil && mount.ID != 0 && mount.MountPoint == s.bootstrap.MountPath && mount.FilesystemType == "fuse.s3fs" {
			return mount, nil
		}
		if err == nil && strings.HasPrefix(mount.FilesystemType, "fuse") {
			return Mount{}, fmt.Errorf("unexpected effective FUSE filesystem")
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Mount{}, fmt.Errorf("wait for workspace mount: %w", ctx.Err())
		case <-deadline.C:
			timer.Stop()
			return Mount{}, fmt.Errorf("workspace mount readiness timed out")
		case <-timer.C:
		}
	}
}

func (s *Supervisor) verifyActiveMountLocked(expectedID uint64) error {
	select {
	case <-s.processDone:
		return fmt.Errorf("s3fs process is not alive")
	default:
	}
	mount, err := s.config.MountInfo()
	if err != nil || mount.ID == 0 || mount.ID != expectedID || mount.MountPoint != s.bootstrap.MountPath || mount.FilesystemType != "fuse.s3fs" {
		return fmt.Errorf("effective workspace mount identity changed")
	}
	return nil
}

func (s *Supervisor) Flush(ctx context.Context, request fuseprotocol.ControlRequest) (fuseprotocol.ControlAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ack := fuseprotocol.ControlAck{Version: fuseprotocol.Version, RuntimeUID: effectiveRuntimeUID(s.bootstrap), Generation: s.auth.LeaseGeneration}
	if err := ctx.Err(); err != nil {
		return ack, err
	}
	if request.Version != fuseprotocol.Version || request.RuntimeUID != ack.RuntimeUID || request.Generation <= 0 || request.Generation != ack.Generation || s.state != StateReady {
		return ack, fmt.Errorf("flush request does not match active mount")
	}
	if err := s.flushLocked(ctx); err != nil {
		return ack, err
	}
	ack.Accepted = true
	return ack, nil
}

func (s *Supervisor) flushLocked(ctx context.Context) error {
	if s.profile.Flush == nil {
		s.state = StateUnhealthy
		return ErrFlushUnsupported
	}
	if s.mountID == 0 {
		s.state = StateUnhealthy
		return fmt.Errorf("active mount identity was not established")
	}
	if err := s.verifyActiveMountLocked(s.mountID); err != nil {
		s.state = StateUnhealthy
		return err
	}
	flushCtx, cancel := context.WithTimeout(ctx, time.Duration(s.bootstrap.FlushTimeoutSeconds)*time.Second)
	defer cancel()
	if err := s.runner.Run(flushCtx, s.profile.Flush(s.bootstrap)); err != nil {
		s.state = StateUnhealthy
		return fmt.Errorf("verified profile flush failed: %w", err)
	}
	if err := s.verifyActiveMountLocked(s.mountID); err != nil {
		s.state = StateUnhealthy
		return err
	}
	return nil
}

func (s *Supervisor) Shutdown(ctx context.Context, request *fuseprotocol.ControlRequest) (fuseprotocol.ShutdownAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation := s.auth.LeaseGeneration
	ack := fuseprotocol.ShutdownAck{Version: fuseprotocol.Version, RuntimeUID: effectiveRuntimeUID(s.bootstrap), Generation: generation}
	if err := ctx.Err(); err != nil {
		return ack, err
	}
	if s.shutdownTried {
		return ack, fmt.Errorf("shutdown was already attempted")
	}
	strong := request != nil
	if strong && (request.Version != fuseprotocol.Version || request.RuntimeUID != ack.RuntimeUID || request.Generation != generation || request.Generation < 0) {
		return ack, fmt.Errorf("shutdown request does not match runtime")
	}
	s.shutdownTried = true
	if s.state == StatePrepared && generation == 0 {
		if s.config.MountInfo == nil {
			s.state = StateUnhealthy
			return ack, fmt.Errorf("effective mount verification is unavailable")
		}
		mount, err := s.config.MountInfo()
		if err != nil || strings.HasPrefix(mount.FilesystemType, "fuse") {
			s.state = StateUnhealthy
			return ack, fmt.Errorf("pristine workspace mount cannot be verified")
		}
		ack.GracefulUnmount = strong
		s.state = StateStopped
		return ack, nil
	}
	if strong {
		if s.state != StateReady {
			return ack, fmt.Errorf("strong shutdown requires a ready mount")
		}
		if err := s.flushLocked(ctx); err != nil {
			return ack, fmt.Errorf("strong shutdown flush failed: %w", err)
		}
	} else if s.state == StateReady && s.profile.Flush != nil {
		_ = s.flushLocked(ctx)
	}
	if s.process == nil {
		return ack, fmt.Errorf("active s3fs process is unavailable")
	}
	s.stopping = true
	unmountCtx, cancel := context.WithTimeout(ctx, time.Duration(s.bootstrap.UnmountTimeoutSeconds)*time.Second)
	defer cancel()
	if err := s.runner.Run(unmountCtx, []string{"/usr/bin/fusermount3", "-u", s.bootstrap.MountPath}); err != nil {
		s.state = StateUnhealthy
		return ack, fmt.Errorf("bounded FUSE unmount failed: %w", err)
	}
	select {
	case <-s.processDone:
	case <-unmountCtx.Done():
		s.state = StateUnhealthy
		return ack, fmt.Errorf("s3fs process did not exit after unmount")
	}
	if s.config.MountInfo == nil {
		s.state = StateUnhealthy
		return ack, fmt.Errorf("effective mount verification is unavailable")
	}
	if mount, err := s.config.MountInfo(); err != nil || mount.FilesystemType == "fuse.s3fs" {
		s.state = StateUnhealthy
		return ack, fmt.Errorf("effective FUSE unmount cannot be verified")
	}
	s.state = StateStopped
	ack.GracefulUnmount = strong
	return ack, nil
}

func (s *Supervisor) statusLocked() fuseprotocol.MounterStatus {
	return fuseprotocol.MounterStatus{Version: fuseprotocol.Version, State: string(s.state), RuntimeUID: effectiveRuntimeUID(s.bootstrap), PoolKey: s.bootstrap.PoolKey, MountType: func() string {
		if s.state == StateReady {
			return "fuse"
		}
		return ""
	}(), Generation: s.auth.LeaseGeneration, RestartDetected: s.state == StateRestartDetected}
}

func (s *Supervisor) Status() fuseprotocol.MounterStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func effectiveRuntimeUID(b fuseprotocol.BootstrapConfig) string { return b.RuntimeUID }
func mustProfile(registry ProfileRegistry, id string) Profile {
	profile, _ := registry.Lookup(id)
	return profile
}
func validHexDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size
}

var bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func validBucket(value string) bool {
	return bucketPattern.MatchString(value) && !strings.Contains(value, "..")
}

func boundedTimeout(seconds int64) bool { return seconds > 0 && seconds <= 3600 }

func boundedBootstrapStrings(value fuseprotocol.BootstrapConfig) bool {
	fields := []string{value.RuntimeUID, value.Provider, value.Bucket, value.Endpoint, value.Region, value.Profile, value.AccessKeyFile, value.SecretKeyFile, value.PasswdFile, value.CAFile, value.CacheDir, value.MountPath, value.PoolKey}
	for _, field := range fields {
		if len(field) > 4096 || !utf8.ValidString(field) {
			return false
		}
		for _, value := range field {
			if value == 0 || unicode.IsControl(value) {
				return false
			}
		}
	}
	return true
}
func withinRoot(path, root string) bool {
	if path == "" || root == "" || !filepath.IsAbs(path) || !filepath.IsAbs(root) || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func withinRootOrSelf(path, root string) bool {
	if path == "" || root == "" || !filepath.IsAbs(path) || !filepath.IsAbs(root) || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func secureDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect trusted directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !ownedByCurrentUser(info) {
		return fmt.Errorf("trusted directory mode is invalid")
	}
	return nil
}

func prepareSecureDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return fmt.Errorf("trusted directory owner or type is invalid")
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("secure trusted directory: %w", err)
	}
	return secureDirectory(path, mode)
}

func prepareCacheTemp(path, root string) error {
	if !withinRoot(path, root) || filepath.Dir(path) == path {
		return fmt.Errorf("cache temporary directory is outside trusted root")
	}
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("create cache temporary directory: %w", err)
	}
	if err := prepareSecureDirectory(path, 0o700); err != nil {
		return err
	}
	return secureResolvedDirectory(path, root)
}

func secureResolvedDirectory(path, root string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve trusted root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve trusted directory: %w", err)
	}
	if !withinRootOrSelf(resolvedPath, resolvedRoot) {
		return fmt.Errorf("resolved directory escapes trusted root")
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("trusted directory is invalid")
	}
	return nil
}

func secureResolvedFile(path, root string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve trusted root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve trusted file")
	}
	if !withinRoot(resolvedPath, resolvedRoot) {
		return fmt.Errorf("resolved file escapes trusted root")
	}
	info, err := os.Stat(resolvedPath)
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("trusted file type or mode is invalid")
	}
	return nil
}

func secureResolvedCredential(path, root string) error {
	if err := secureResolvedFile(path, root); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve credential source")
	}
	info, err := os.Stat(resolved)
	if err != nil || (info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) {
		return fmt.Errorf("credential source mode is invalid")
	}
	return nil
}

func securePublishedFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		return fmt.Errorf("published file type, mode, or owner is invalid")
	}
	return nil
}

func readBoundedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open persisted supervisor state")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, fuseprotocol.MaxJSONBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("persisted supervisor state has invalid size")
	}
	return raw, nil
}
func readCredential(path, root string, access bool) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !withinRoot(resolved, mustResolveRoot(root)) {
		return nil, fmt.Errorf("read credential source")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("read credential source")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || (info.Mode().Perm() != 0o400 && info.Mode().Perm() != 0o600) {
		return nil, fmt.Errorf("read credential source")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, fmt.Errorf("read credential source")
	}
	if len(raw) == 0 || len(raw) > 4096 {
		wipe(raw)
		return nil, fmt.Errorf("credential source size is invalid")
	}
	raw = bytesTrimOneNewline(raw)
	if len(raw) == 0 || containsCredentialDelimiter(raw, access) {
		wipe(raw)
		return nil, fmt.Errorf("credential source format is invalid")
	}
	return raw, nil
}

func mustResolveRoot(root string) string {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	return resolved
}

func containsCredentialDelimiter(raw []byte, access bool) bool {
	for _, value := range raw {
		if value == '\r' || value == '\n' || value == 0 || (access && value == ':') {
			return true
		}
	}
	return false
}
func bytesTrimOneNewline(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		return raw[:len(raw)-1]
	}
	return raw
}
func wipe(raw []byte) {
	for i := range raw {
		raw[i] = 0
	}
}

func atomicPublish(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".tmp-publish-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err = temp.Write(data); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func createExclusiveSynced(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
