package workspaceprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	ioHelperEnvironmentName  = "WORKSPACE_PROBE_INTERNAL_IO_HELPER_V1"
	ioHelperEnvironmentValue = "run"
	ioHelperStateFD          = 3
	workspaceIOTimeout       = 15 * time.Second
	workspaceIOKillGrace     = 2 * time.Second
	workspaceIOPollInterval  = 10 * time.Millisecond
)

type ioHelperLaunch struct {
	Version    int    `json:"version"`
	Operation  string `json:"operation"`
	RuntimeUID string `json:"runtime_uid"`
	Generation int64  `json:"generation"`
	ProbeName  string `json:"probe_name"`
}

type ioChild interface {
	poll() (bool, error)
	kill() error
	release() error
}

type subprocessIOBoundary struct {
	start        func(operation, runtimeUID string, generation int64, probeName string) (ioChild, error)
	timeout      time.Duration
	killGrace    time.Duration
	pollInterval time.Duration
	hold         func()
}

func newSubprocessIOBoundary() *subprocessIOBoundary {
	return &subprocessIOBoundary{
		start: startWorkspaceIOHelper, timeout: workspaceIOTimeout,
		killGrace: workspaceIOKillGrace, pollInterval: workspaceIOPollInterval,
		hold: func() { select {} },
	}
}

func (b *subprocessIOBoundary) run(runtimeUID string, generation int64) error {
	probeName, err := fuseprotocol.DeriveProbeObjectName(runtimeUID, generation)
	if err != nil {
		return fmt.Errorf("generate bounded workspace probe nonce: %w", err)
	}
	primaryErr := b.runChild("probe", runtimeUID, generation, probeName)
	if primaryErr == nil {
		return nil
	}
	cleanupErr := b.runChild("cleanup", runtimeUID, generation, probeName)
	if cleanupErr != nil {
		// Do not return control to a caller that could mistake runtime teardown
		// for optional cleanup. Production hold never returns; the runtime's
		// outer deadline destroys this single-use sandbox and its PID namespace.
		b.hold()
		return errors.Join(primaryErr, ErrIOCompensationUnconfirmed, cleanupErr)
	}
	return primaryErr
}

func (b *subprocessIOBoundary) runChild(operation, runtimeUID string, generation int64, probeName string) error {
	child, err := b.start(operation, runtimeUID, generation, probeName)
	if err != nil {
		return fmt.Errorf("start bounded workspace probe %s: %w", operation, err)
	}
	return waitBoundedIO(child, b.timeout, b.killGrace, b.pollInterval)
}

func waitBoundedIO(child ioChild, timeout, killGrace, pollInterval time.Duration) error {
	if timeout <= 0 || killGrace <= 0 || pollInterval <= 0 {
		_ = child.release()
		return fmt.Errorf("invalid workspace probe I/O timeout")
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := child.poll()
		if done {
			_ = child.release()
			return err
		}
		if err != nil {
			return terminateIOChild(child, err, killGrace, pollInterval)
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(minDuration(pollInterval, time.Until(deadline)))
	}
	return terminateIOChild(child, ErrIOTimeout, killGrace, pollInterval)
}

func terminateIOChild(child ioChild, cause error, killGrace, pollInterval time.Duration) error {
	killErr := child.kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	killDeadline := time.Now().Add(killGrace)
	for {
		done, _ := child.poll()
		if done {
			_ = child.release()
			if killErr != nil {
				return errors.Join(cause, fmt.Errorf("kill workspace probe I/O helper: %w", killErr))
			}
			return cause
		}
		// Once the normal kill grace expires, deliberately keep polling without
		// releasing. An uninterruptible FUSE task remains this probe's child and
		// wait responsibility until the runtime tears down the whole sandbox.
		delay := pollInterval
		if remaining := time.Until(killDeadline); remaining > 0 && remaining < delay {
			delay = remaining
		}
		time.Sleep(delay)
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func startWorkspaceIOHelper(operation, runtimeUID string, generation int64, probeName string) (ioChild, error) {
	launch := ioHelperLaunch{Version: fuseprotocol.Version, Operation: operation, RuntimeUID: runtimeUID, Generation: generation, ProbeName: probeName}
	raw, err := json.Marshal(launch)
	if err != nil || len(raw) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("encode workspace probe I/O helper state")
	}
	stateReader, stateWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer stateReader.Close()
	defer stateWriter.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	argv := []string{
		fuseprotocol.ProbeBinary, "write-read-delete", "--runtime-uid", runtimeUID,
		"--generation", strconv.FormatInt(generation, 10),
	}
	process, err := os.StartProcess(executable, argv, &os.ProcAttr{
		Dir:   "/",
		Env:   ioHelperEnvironment(),
		Files: []*os.File{devNull, devNull, devNull, stateReader},
		Sys:   ioHelperProcessAttributes(),
	})
	if err != nil {
		return nil, err
	}
	child := newOSIOChild(process)
	_ = stateReader.Close()
	if err := writeAll(stateWriter, raw); err != nil {
		_ = child.kill()
		_ = waitBoundedIO(child, time.Millisecond, workspaceIOKillGrace, workspaceIOPollInterval)
		return nil, err
	}
	_ = stateWriter.Close()
	return child, nil
}

func ioHelperEnvironment() []string {
	result := make([]string, 0, len(os.Environ())+1)
	prefix := ioHelperEnvironmentName + "="
	for _, item := range os.Environ() {
		if strings.HasPrefix(item, prefix) || strings.HasPrefix(item, brokerEnvironment+"=") {
			continue
		}
		result = append(result, item)
	}
	return append(result, ioHelperEnvironmentName+"="+ioHelperEnvironmentValue)
}

// MaybeRunIOHelper enters the non-public, fixed operation subprocess used to
// bound FUSE syscalls. The inherited state fd and exact public argv must agree.
func MaybeRunIOHelper() (bool, int) {
	if os.Getenv(ioHelperEnvironmentName) != ioHelperEnvironmentValue {
		return false, 0
	}
	stateFile := os.NewFile(ioHelperStateFD, "workspace-probe-io-state")
	if stateFile == nil {
		return true, 1
	}
	defer stateFile.Close()
	var launch ioHelperLaunch
	if err := decodeLimitedJSON(stateFile, &launch); err != nil ||
		!validIOHelperInvocation(os.Geteuid(), os.Getegid(), os.Getenv(ioHelperEnvironmentName), os.Args, launch) {
		return true, 1
	}
	_ = stateFile.Close()
	if err := os.Chdir("/"); err != nil {
		return true, 1
	}
	probe := New()
	var err error
	switch launch.Operation {
	case "probe":
		err = probe.writeReadDeleteDirect(launch.RuntimeUID, launch.Generation, launch.ProbeName)
	case "cleanup":
		err = probe.cleanupProbeDirect(launch.RuntimeUID, launch.Generation, launch.ProbeName)
	}
	if err != nil {
		return true, 1
	}
	return true, 0
}

func validIOHelperInvocation(uid, gid int, environment string, argv []string, launch ioHelperLaunch) bool {
	if uid != requiredUID || gid != requiredGID || environment != ioHelperEnvironmentValue ||
		launch.Version != fuseprotocol.Version || (launch.Operation != "probe" && launch.Operation != "cleanup") ||
		!fuseprotocol.ValidIdentity(launch.RuntimeUID) || launch.Generation <= 0 {
		return false
	}
	expectedProbeName, err := fuseprotocol.DeriveProbeObjectName(launch.RuntimeUID, launch.Generation)
	if err != nil || launch.ProbeName != expectedProbeName {
		return false
	}
	expected := []string{
		fuseprotocol.ProbeBinary, "write-read-delete", "--runtime-uid", launch.RuntimeUID,
		"--generation", strconv.FormatInt(launch.Generation, 10),
	}
	if len(argv) != len(expected) {
		return false
	}
	for index := range expected {
		if argv[index] != expected[index] {
			return false
		}
	}
	return true
}
