package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/mounter"
)

const (
	runDir         = "/run/s3fs"
	credentialRoot = "/run/secrets/workspace"
	cacheRoot      = "/var/cache/s3fs"
	mountPath      = "/workspace"
	controlSocket  = "/run/s3fs/control.sock"
)

func main() {
	if err := run(os.Args, os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "workspace-mounter: request failed")
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	return runWithSocket(args, stdin, stdout, controlSocket)
}

func runWithSocket(args []string, stdin io.Reader, stdout io.Writer, socketPath string) error {
	if len(args) == 0 {
		return fmt.Errorf("invalid workspace-mounter command")
	}
	canonical := append([]string{fuseprotocol.MounterBinary}, args[1:]...)
	if !fuseprotocol.AllowedMounterCLI(canonical) {
		return fmt.Errorf("invalid workspace-mounter command")
	}
	if len(args) == 2 && args[1] == "supervise" {
		return supervise()
	}
	if len(args) == 4 && args[1] == "health" && args[2] == "prepared" && args[3] == "--self-check-image" {
		return mounter.CheckImage(runDir, cacheRoot)
	}
	input, err := readBounded(stdin)
	if err != nil {
		return err
	}
	command := args[1]
	switch command {
	case "health":
		if len(input) != 0 {
			return fmt.Errorf("health does not accept input")
		}
		command = "health-" + args[2]
		input = []byte(`{}`)
	case "shutdown":
		if len(bytes.TrimSpace(input)) == 0 {
			command = "shutdown-best-effort"
			input = []byte(`{}`)
		}
	case "bootstrap", "authorize", "flush":
		if len(bytes.TrimSpace(input)) == 0 {
			return fmt.Errorf("control command requires input")
		}
	}
	output, err := (mounter.Client{SocketPath: socketPath, IOTimeout: 10 * time.Second}).Do(context.Background(), command, input)
	if err != nil {
		return err
	}
	if command == "shutdown-best-effort" {
		return nil
	}
	if len(output) == 0 || len(output) > fuseprotocol.MaxJSONBytes {
		return fmt.Errorf("invalid supervisor output")
	}
	if command == "health-prepared" || command == "health-ready" {
		if err := validateHealthOutput(command, output); err != nil {
			return err
		}
	}
	_, err = stdout.Write(output)
	return err
}

func validateHealthOutput(command string, output []byte) error {
	var status fuseprotocol.MounterStatus
	if err := fuseprotocol.DecodeExact(output, &status); err != nil {
		return fmt.Errorf("invalid supervisor health output")
	}
	return validateHealthStatus(command, status)
}

func validateHealthStatus(command string, status fuseprotocol.MounterStatus) error {
	if status.Version != fuseprotocol.Version || status.RestartDetected {
		return fmt.Errorf("supervisor health state is not ready")
	}
	switch command {
	case "health-prepared":
		if status.State != "prepared" || status.MountType != "" || status.Generation != 0 {
			return fmt.Errorf("supervisor is not prepared")
		}
	case "health-ready":
		if status.State != "ready" || status.MountType != "fuse" || status.Generation <= 0 {
			return fmt.Errorf("supervisor is not ready")
		}
	default:
		return fmt.Errorf("invalid supervisor health command")
	}
	return nil
}

func supervise() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("supervisor must be container PID 1")
	}
	supervisor := mounter.NewSupervisor(mounter.Config{
		RunDir: runDir, CredentialRoot: credentialRoot, CacheRoot: cacheRoot, MountPath: mountPath,
		CheckFuse: mounter.CheckFuseDevice, PrepareAnchor: mounter.PrepareWorkspaceAnchor, CheckAnchor: mounter.CheckWorkspaceAnchor,
		MountInfo: func() (mounter.Mount, error) { return mounter.ReadEffectiveMount(mountPath) },
	}, mounter.CommandRunner{})
	expectedUID := os.Getenv("SANDBOX_RUNTIME_UID")
	rawBootstrap := os.Getenv("SANDBOX_MOUNTER_BOOTSTRAP")
	if supervisor.State() == mounter.StateRestartDetected {
		if rawBootstrap == "" && expectedUID == "" {
			// Docker recovery trusts only the root-owned, mode-0600 persisted
			// identity because the immutable container ID is not an environment.
			_ = supervisor.RecoverRestarted(fuseprotocol.BootstrapConfig{}, "")
		} else if rawBootstrap != "" && len(rawBootstrap) <= fuseprotocol.MaxJSONBytes {
			var bootstrap fuseprotocol.BootstrapConfig
			if fuseprotocol.Decode([]byte(rawBootstrap), &bootstrap) == nil {
				_ = supervisor.RecoverRestarted(bootstrap, expectedUID)
			}
		}
		// Recovery is deliberately terminal and read-only. Failure leaves the
		// supervisor serving fail-closed health/control responses for fencing.
	} else if rawBootstrap != "" {
		if len(rawBootstrap) > fuseprotocol.MaxJSONBytes {
			return fmt.Errorf("bootstrap environment exceeds limit")
		}
		var bootstrap fuseprotocol.BootstrapConfig
		if err := fuseprotocol.Decode([]byte(rawBootstrap), &bootstrap); err != nil {
			return err
		}
		if err := supervisor.Bootstrap(context.Background(), bootstrap, expectedUID); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	server := mounter.Server{Supervisor: supervisor, SocketPath: filepath.Clean(controlSocket), ExpectedRuntimeUID: expectedUID, IOTimeout: 10 * time.Second}
	if err := server.Serve(ctx); err != nil {
		return err
	}
	teardownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = supervisor.Shutdown(teardownCtx, nil)
	return nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, fuseprotocol.MaxJSONBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read control input")
	}
	if len(raw) > fuseprotocol.MaxJSONBytes {
		return nil, fmt.Errorf("control input exceeds limit")
	}
	return raw, nil
}
